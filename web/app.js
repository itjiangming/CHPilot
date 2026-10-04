/* CHPilot 前端逻辑：状态以 WebSocket 推送为准，偏好经 localStorage 持久化。 */
"use strict";

const $ = (id) => document.getElementById(id);
const LS_KEY = "chpilot.state";

// 旧版本（WebCom）本地设置迁移：键前缀 webcom.* → chpilot.*，避免升级后配置丢失
(function migrateLegacyStorage() {
  try {
    const pairs = { "webcom.state": "chpilot.state", "webcom.clientId": "chpilot.clientId" };
    for (const [oldKey, newKey] of Object.entries(pairs)) {
      const val = localStorage.getItem(oldKey);
      if (val !== null) {
        if (localStorage.getItem(newKey) === null) localStorage.setItem(newKey, val);
        localStorage.removeItem(oldKey);
      }
    }
  } catch (e) { /* 忽略迁移失败，按全新安装处理 */ }
})();

// 客户端身份：每个 GUI 实例持久化一个标识，用于多实例操作提示（谁做了什么）
const CLIENT_ID = (() => {
  let id = localStorage.getItem("chpilot.clientId");
  if (!id) {
    id = "GUI-" + Math.random().toString(16).slice(2, 6).toUpperCase();
    localStorage.setItem("chpilot.clientId", id);
  }
  return id;
})();

const state = {
  portOpen: false,
  channelCount: 1,   // 路数：本 GUI 实例的本地展示偏好（各客户端独立，服务端不保存）
  wiringMap: {},     // { "1": "no"|"nc" } 每通道本地接线视角（仅本 GUI 实例，服务端无此状态）
  states: {},        // { "1": true, ... }
  unknown: [],       // ["2", ...]
  seqRunning: false,
  monitorOpen: false,
  advCards: {},      // 每通道高级按钮（开/关/切换）显示状态，默认隐藏
  savedCfg: {},      // 本地保存的串口配置（含 selPort，枚举填充后据此恢复选择）
  wsDown: false,     // WebSocket 断线标志
  ws: null,          // 当前 WebSocket 实例（防重复建连）
  clientIp: "",      // 本实例客户端 IP（服务端随 /api/status 返回）
  version: "",       // 服务端版本号（随 /api/status 返回）
  relaySeq: 0,       // WS relay 广播计数：用于丢弃过期的一次性状态快照
  monLines: [],      // 环形缓冲，最多 1000 条
  monAutoScroll: true,
};

/* ---------- localStorage ---------- */
function loadPrefs() {
  try {
    const p = JSON.parse(localStorage.getItem(LS_KEY) || "{}");
    if (p.cfg) {
      state.savedCfg = p.cfg;
      for (const id of ["baudRate", "dataBits", "stopBits", "parity", "flowControl"]) {
        if (p.cfg[id] !== undefined && p.cfg[id] !== "") $(id).value = p.cfg[id];
      }
      // selPort 不在此处赋值：此刻下拉列表尚未枚举填充，赋值会静默丢失
      // （renderPortOptions 会依据 state.savedCfg.selPort 恢复）
    }
    if (p.channelCount) {
      // 路数为本地偏好：下拉框与内存状态必须同步，否则渲染张数与显示值不一致
      $("channelCount").value = p.channelCount;
      state.channelCount = parseInt(p.channelCount) || 1;
    }
    if (p.channelWiring) {
      state.wiringMap = p.channelWiring;
    }
    if (p.monMode) $("monMode").value = p.monMode;
    if (Array.isArray(p.cmds)) p.cmds.forEach(addCmdRow);
    if (p.loopMode === "inf") $("loopCount").parentElement.querySelector('input[value="inf"]').checked = true;
    if (p.monitorOpen) toggleMonitor(true);
  } catch (e) { /* 忽略坏数据 */ }
  if ($("cmdList").children.length === 0) addCmdRow({ hex: "A0 01 05 A6", delayMs: 200 });
}

function savePrefs() {
  const cfg = {};
  for (const id of ["selPort", "baudRate", "dataBits", "stopBits", "parity", "flowControl"]) {
    cfg[id] = $(id).value;
  }
  const cmds = [...$("cmdList").children].map((row) => ({
    hex: row.querySelector(".hex-in").value,
    delayMs: parseInt(row.querySelector(".delay-in").value) || 0,
  }));
  localStorage.setItem(LS_KEY, JSON.stringify({
    cfg,
    channelCount: $("channelCount").value,
    channelWiring: state.wiringMap,
    monMode: $("monMode").value,
    cmds,
    loopMode: document.querySelector('input[name="loopMode"]:checked').value,
    monitorOpen: state.monitorOpen,
  }));
}

/* ---------- 通用 ---------- */
async function api(path, body, method) {
  const headers = { "X-Client-Id": CLIENT_ID };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const opt = body !== undefined
    ? { method: method || "POST", headers, body: JSON.stringify(body) }
    : { method: method || "GET", headers };
  const resp = await fetch(path, opt);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
  return data;
}

function setDisabledWithin(card, on, exceptIds) {
  card.querySelectorAll("button, select, input").forEach((el) => {
    if (!(exceptIds || []).includes(el.id)) el.disabled = !on;
  });
}

function setPanelEnabled(on) {
  // 路数是本地展示偏好，未连接时也可先设置；监视按钮同理始终可用
  setDisabledWithin($("cardPanel"), on, ["btnMonitor", "channelCount"]);
  setDisabledWithin($("cardSeq"), on);
  // 监视窗口仅禁用发送，展开/切换显示模式始终可用
  $("monInput").disabled = !on;
  $("btnSend").disabled = !on;
  $("cardPanel").classList.toggle("disabled", !on);
  $("cardSeq").classList.toggle("disabled", !on);
  $("btnOpen").textContent = on ? "关闭串口" : "打开串口";
  $("portLamp").className = "lamp " + (on ? "on" : "off");
  $("portLabel").textContent = on ? `已连接 ${state.portName || ""}` : "串口未连接";
}

/* ---------- 串口连接 ---------- */
// renderPortOptions 重建端口下拉框，尽量保持当前选中项；串口已打开时保证该项在列表中。
function renderPortOptions(ports) {
  const sel = $("selPort");
  const openName = state.portOpen ? state.portName : "";
  // 恢复优先级：当前选中 > 已打开端口 > 本地上次使用的端口 > CH340 设备 > 第一项
  const preferred = [sel.value, openName, (state.savedCfg && state.savedCfg.selPort) || ""].filter(Boolean);
  sel.innerHTML = "";
  const items = ports.slice();
  if (openName && !items.some((p) => p.name === openName)) {
    items.push({ name: openName, isCH340: false });
  }
  if (!items.length) {
    sel.innerHTML = `<option value="">（未发现串口设备，插入设备后将自动出现）</option>`;
    return;
  }
  for (const p of items) {
    const opt = document.createElement("option");
    opt.value = p.name;
    opt.textContent = p.isCH340 ? `${p.name}（CH340）` : p.name;
    sel.appendChild(opt);
  }
  let picked = preferred.find((v) => [...sel.options].some((o) => o.value === v));
  if (!picked) {
    const ch340 = items.find((p) => p.isCH340);
    picked = ch340 ? ch340.name : items[0].name;
  }
  sel.value = picked;
  hideOpenErr();
}

async function refreshPorts() {
  try {
    const data = await api("/api/ports");
    renderPortOptions(data.ports || []);
    if (!data.ports.length && data.hint) showOpenErr(data.hint);
  } catch (e) {
    showOpenErr("枚举串口失败: " + e.message);
  }
}

function showOpenErr(msg) { $("openErr").textContent = msg; $("openErr").classList.remove("hidden"); }
function hideOpenErr() { $("openErr").classList.add("hidden"); }

async function togglePort() {
  const btn = $("btnOpen");
  if (btn.disabled) return; // 防连点
  if (state.portOpen) {
    btn.disabled = true;
    btn.textContent = "关闭中…";
    try {
      await api("/api/close", {});
      // 乐观更新，随后的 port 广播会覆盖为权威状态
      state.portOpen = false;
      state.portName = "";
      state.states = {};
      state.unknown = [];
      setPanelEnabled(false);
      renderChannels();
    } catch (e) {
      showOpenErr("关闭失败: " + e.message);
    } finally {
      btn.disabled = false;
      btn.textContent = state.portOpen ? "关闭串口" : "打开串口";
    }
    return;
  }
  hideOpenErr();
  if (!$("selPort").value) { showOpenErr("请先选择串口设备"); return; }
  btn.disabled = true;
  btn.textContent = "打开中…";
  try {
    await api("/api/open", {
      port: $("selPort").value,
      baudRate: parseInt($("baudRate").value),
      dataBits: parseInt($("dataBits").value),
      stopBits: parseInt($("stopBits").value),
      parity: $("parity").value,
      flowControl: $("flowControl").value,
      channelCount: state.channelCount, // 打开后按本实例路数逐路查询
    });
    // 乐观更新：立即点亮状态灯，逐路状态查询完成后由 relay 广播刷新
    state.portOpen = true;
    state.portName = $("selPort").value;
    setPanelEnabled(true);
  } catch (e) {
    showOpenErr(e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = state.portOpen ? "关闭串口" : "打开串口";
  }
}

/* ---------- 控制面板 ---------- */
// pendingRelay 正在执行控制请求的通道，执行期间禁用该通道开关防连点错位
const pendingRelay = new Set();

function renderChannels() {
  const grid = $("channelGrid");
  grid.innerHTML = "";
  for (let ch = 1; ch <= state.channelCount; ch++) {
    const key = String(ch);
    // 从未查询过（states 中无该通道）或查询超时 → 状态未知，不能想当然显示"逻辑关"
    const known = Object.prototype.hasOwnProperty.call(state.states, key);
    const unknown = !known || state.unknown.includes(key);
    const physOn = state.states[key] === true; // 物理状态：true=01 线圈吸合（NO 通）
    const wiring = state.wiringMap[key] || "nc";
    // 逻辑状态按本 GUI 实例的接线视角从物理状态推导：
    // NC 视角（受控线=常闭端）：物理关=逻辑开；NO 视角：物理开=逻辑开
    const on = wiring === "nc" ? !physOn : physOn;
    const adv = !!state.advCards[key];
    const card = document.createElement("div");
    card.className = "ch-card" + (on && !unknown ? " on" : "");
    card.innerHTML = `
      <div class="ch-head">
        <span class="lamp ${on && !unknown ? "on" : "off"}"></span>
        <span class="ch-name">通道 ${ch}</span>
        <button class="ch-menu${adv ? " active" : ""}" data-ch="${ch}" title="展开/收起 开/关/切换 按钮">▸</button>
      </div>
      <div class="ch-row">
        <span class="ch-label">状态</span>
        <span class="ch-state">${unknown ? "状态未知" : `${on ? "逻辑开" : "逻辑关"}（物理${physOn ? "01" : "00"}）`}</span>
      </div>
      <div class="ch-row">
        <span class="ch-label">接线</span>
        <select class="wiring-sel" data-ch="${ch}" title="接线视角（本实例本地设置，每路独立）">
          <option value="no" ${wiring === "no" ? "selected" : ""}>NO 常开</option>
          <option value="nc" ${wiring === "nc" ? "selected" : ""}>NC 常闭</option>
        </select>
      </div>
      <div class="ch-row">
        <span class="ch-label">主控</span>
        <label class="switch" title="主控开关（逻辑开/关）">
          <input type="checkbox" class="sw-input" data-ch="${ch}" ${on && !unknown ? "checked" : ""} ${pendingRelay.has(key) ? "disabled" : ""}>
          <span class="slider"></span>
        </label>
      </div>
      <div class="ch-adv ${adv ? "" : "hidden"}">
        <span class="ch-adv-hint">独立指令下发：</span>
        <div class="ch-btns">
          <button data-act="on" data-ch="${ch}">开</button>
          <button data-act="off" data-ch="${ch}">关</button>
          <button data-act="toggle" data-ch="${ch}" class="ghost">切换</button>
        </div>
      </div>`;
    // 状态未知：switch 显示中间态（灰蓝滑块），避免误读为"已确认关"
    if (unknown) card.querySelector(".sw-input").indeterminate = true;
    grid.appendChild(card);
  }
}

async function relayAction(action, channel) {
  const key = String(channel);
  if (pendingRelay.has(key)) return;
  pendingRelay.add(key);
  renderChannels(); // 立即禁用该通道开关
  try {
    const payload = { channel, action };
    if (action === "on" || action === "off") {
      // on/off 是逻辑语义，随请求携带本实例的接线视角，服务端无状态映射
      payload.wiring = state.wiringMap[key] || "nc";
    }
    const resp = await api("/api/relay", payload);
    // 响应为物理状态，逻辑显示由本地视角推导；
    // 已确认物理状态的通道要从 unknown 中移除，否则成功后仍显示"状态未知"
    if (resp.states) {
      state.states = resp.states;
      state.unknown = state.unknown.filter((k) => !(k in resp.states));
    }
  } catch (e) {
    showOpenErr(e.message);
    setTimeout(hideOpenErr, 5000);
  } finally {
    pendingRelay.delete(key);
    renderChannels(); // 依据服务端状态重绘，checkbox 与真实状态强同步
  }
}

// setChannelWiring 切换本 GUI 实例的接线视角：纯本地操作，即时生效。
// 物理状态由服务端统一维护，逻辑显示按本地视角实时重映射（服务重启后依然有效）。
function setChannelWiring(channel, wiring) {
  state.wiringMap[String(channel)] = wiring;
  renderChannels();
  savePrefs();
}

async function relayAll(action) {
  try {
    // "全部开启/关闭"按本实例各通道接线视角映射为物理指令
    await api("/api/relay/all", {
      action,
      wiring: { ...state.wiringMap },
      channelCount: state.channelCount,
    });
  }
  catch (e) { alert("执行失败: " + e.message); }
}

async function refreshStatus() {
  const seqBefore = state.relaySeq;
  try {
    const st = await api("/api/status");
    // 请求期间若已收到更新的 WS relay 广播（状态更权威且更新），丢弃这份快照。
    // 否则会出现：WS 新状态先渲染 → 迟到的 /api/status 旧快照把它覆盖回旧值
    //（页面加载/重连时两者并发，Firefox 时序下偶发触发）
    if (state.relaySeq !== seqBefore) return;
    Object.assign(state, {
      portOpen: st.portOpen, portName: st.portName,
      states: st.states || {}, unknown: st.unknown || [],
      seqRunning: st.seqRunning,
      clientIp: st.clientIp || state.clientIp || "",
      version: st.version || state.version || "",
    });
    if (state.version) $("aboutVersion").textContent = state.version;
    updateClientLabel();
    setPanelEnabled(st.portOpen);
    renderChannels();
    renderSeqUI();
  } catch (e) { /* 服务未就绪时静默 */ }
}

/* ---------- 数据监视窗口 ---------- */
function toggleMonitor(forceOpen) {
  state.monitorOpen = forceOpen !== undefined ? forceOpen : !state.monitorOpen;
  $("cardMonitor").classList.toggle("hidden", !state.monitorOpen);
  $("btnMonitor").textContent = state.monitorOpen ? "数据监视窗口 ▾" : "数据监视窗口 ▸";
  savePrefs();
}

function renderMonLine(l) {
  const mode = $("monMode").value;
  const div = document.createElement("div");
  div.className = `mon-line ${l.dir}`;
  const dirText = l.dir === "tx" ? "TX ▲" : "RX ▼";
  const content = mode === "hex" ? l.hex : l.text;
  div.innerHTML = `<span class="ts">${l.ts}</span><span class="${l.dir === "tx" ? "dir-tx" : "dir-rx"}">${dirText}</span><span class="hex"></span>`;
  div.querySelector(".hex").textContent = content;
  return div;
}

function pushMonLine(l) {
  state.monLines.push(l);
  if (state.monLines.length > 1000) state.monLines.shift();
  const log = $("monLog");
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  log.appendChild(renderMonLine(l));
  while (log.children.length > 1000) log.removeChild(log.firstChild);
  $("monCount").textContent = `共 ${state.monLines.length} 条`;
  if (atBottom && state.monAutoScroll) log.scrollTop = log.scrollHeight;
}

function rerenderMonitor() {
  const log = $("monLog");
  log.innerHTML = "";
  const frag = document.createDocumentFragment();
  state.monLines.forEach((l) => frag.appendChild(renderMonLine(l)));
  log.appendChild(frag);
  log.scrollTop = log.scrollHeight;
}

/* ---------- 自定义指令序列 ---------- */
function addCmdRow(cmd) {
  const row = document.createElement("div");
  row.className = "cmd-row";
  row.innerHTML = `
    <span class="idx"></span>
    <input class="hex-in" type="text" spellcheck="false" placeholder="HEX 指令，如 A0 01 03 A4">
    <input class="delay-in" type="number" min="0" step="50" value="200" title="发送后延迟（毫秒）">
    <span class="muted">ms</span>
    <button class="ghost up" title="上移">↑</button>
    <button class="ghost down" title="下移">↓</button>
    <button class="ghost del" title="删除">✕</button>`;
  row.querySelector(".hex-in").value = (cmd && cmd.hex) || "";
  row.querySelector(".delay-in").value = (cmd && cmd.delayMs) || 200;
  row.querySelector(".up").onclick = () => { const p = row.previousElementSibling; if (p) $("cmdList").insertBefore(row, p); renumber(); savePrefs(); };
  row.querySelector(".down").onclick = () => { const n = row.nextElementSibling; if (n) $("cmdList").insertBefore(n, row); renumber(); savePrefs(); };
  row.querySelector(".del").onclick = () => { row.remove(); renumber(); savePrefs(); };
  row.querySelector(".hex-in").onchange = savePrefs;
  row.querySelector(".delay-in").onchange = savePrefs;
  $("cmdList").appendChild(row);
  renumber();
}

function renumber() {
  [...$("cmdList").children].forEach((row, i) => {
    row.querySelector(".idx").textContent = i + 1;
  });
}

function renderSeqUI() {
  $("btnSeqStart").classList.toggle("hidden", state.seqRunning);
  $("btnSeqStop").classList.toggle("hidden", !state.seqRunning);
}

async function startSequence() {
  const cmds = [...$("cmdList").children].map((row) => ({
    hex: row.querySelector(".hex-in").value,
    delayMs: parseInt(row.querySelector(".delay-in").value) || 0,
  }));
  if (!cmds.length) { alert("请先添加指令"); return; }
  const loops = document.querySelector('input[name="loopMode"]:checked').value === "inf"
    ? 0
    : Math.max(1, parseInt($("loopCount").value) || 1);
  try {
    await api("/api/sequence", { action: "start", loops, commands: cmds });
    state.seqRunning = true;
    renderSeqUI();
  } catch (e) {
    alert("启动失败: " + e.message);
  }
}

/* ---------- WebSocket ---------- */
function connectWS() {
  if (state.ws && (state.ws.readyState === WebSocket.CONNECTING || state.ws.readyState === WebSocket.OPEN)) {
    return; // 已有存活连接/正在连接，避免重复建连
  }
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws`);
  state.ws = ws;

  ws.onopen = () => {
    // 恢复正常：清除断线提示，拉取权威状态与最新端口列表
    const wasDown = state.wsDown;
    state.wsDown = false;
    hideOpenErr();
    refreshStatus();
    if (wasDown) refreshPorts();
  };

  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch (e) {
      return;
    }
    try {
      handleWsMessage(msg);
    } catch (e) {
      // 单条消息处理异常不应中断后续状态同步（防御性隔离）
      console.error("WS 消息处理异常", msg && msg.type, e);
    }
  };

  ws.onclose = () => {
    // 断线显性提示：状态灯同步变灰、面板禁用，避免"服务已终止但界面仍可点"的误导
    state.wsDown = true;
    state.ws = null;
    showOpenErr("与服务器的连接已断开，正在重连…");
    $("portLamp").className = "lamp off";
    $("portLabel").textContent = "服务连接已断开";
    setPanelEnabled(false);
    // 后台标签页的定时器会被浏览器节流，故另由 visibilitychange/focus 立即重连
    setTimeout(connectWS, 2000);
  };
}

function handleWsMessage(msg) {
  switch (msg.type) {
    case "port":
      state.portOpen = msg.open;
      state.portName = msg.portName || "";
      if (msg.error)
        showOpenErr(msg.error);
      else hideOpenErr();
      setPanelEnabled(msg.open);
      if (!msg.open) {
        state.states = {};
        state.unknown = [];
        renderChannels();
      }
      break;
    case "ports":
      renderPortOptions(msg.ports || []);
      break;
    case "relay":
      state.relaySeq++;
      state.states = msg.states || {}; // 物理状态，逻辑显示按本地接线视角推导
      state.unknown = msg.unknown || [];
      // 路数为本地偏好，不由服务端下发覆盖
      renderChannels();
      break;
    case "data":
      pushMonLine(msg);
      break;
    case "op":
      showToast(
        `${String(msg.client || "未知客户端").startsWith(CLIENT_ID) ? "本机 " : ""}${msg.client} ${msg.action}`
      );
      break;
    case "seq":
      state.seqRunning = msg.running;
      renderSeqUI();
      $("seqProgress").textContent = msg.running
        ? `轮次 ${msg.loop}/${msg.totalLoops || "∞"}`
        : "已停止";
      break;
  }
}

/* ---------- 事件绑定 ---------- */
$("btnRefresh").onclick = refreshPorts;
$("btnOpen").onclick = togglePort;
$("btnAllOn").onclick = () => relayAll("on");
$("btnAllOff").onclick = () => relayAll("off");
$("btnQuery").onclick = async () => {
  try { await api("/api/query", { channelCount: state.channelCount }); }
  catch (e) { showOpenErr(e.message); setTimeout(hideOpenErr, 5000); }
};
$("channelGrid").addEventListener("click", (e) => {
  const menu = e.target.closest(".ch-menu");
  if (menu) {
    const key = menu.dataset.ch;
    state.advCards[key] = !state.advCards[key];
    renderChannels();
    return;
  }
  const btn = e.target.closest("button[data-act]");
  if (btn) relayAction(btn.dataset.act, parseInt(btn.dataset.ch));
});
$("channelGrid").addEventListener("change", (e) => {
  if (e.target.classList.contains("sw-input")) {
    relayAction(e.target.checked ? "on" : "off", parseInt(e.target.dataset.ch));
  } else if (e.target.classList.contains("wiring-sel")) {
    setChannelWiring(parseInt(e.target.dataset.ch), e.target.value);
  }
});
$("channelCount").onchange = async (e) => {
  // 路数只影响本实例的展示范围：立即本地重绘；串口已开时按新路数补查一轮状态
  state.channelCount = parseInt(e.target.value);
  renderChannels();
  savePrefs();
  if (state.portOpen) {
    try { await api("/api/query", { channelCount: state.channelCount }); }
    catch (err) { showOpenErr(err.message); setTimeout(hideOpenErr, 5000); }
  }
};
$("btnMonitor").onclick = () => toggleMonitor();
$("monMode").onchange = () => { rerenderMonitor(); savePrefs(); };
$("btnClearMon").onclick = () => { state.monLines = []; $("monLog").innerHTML = ""; $("monCount").textContent = ""; };
$("monLog").addEventListener("scroll", (e) => {
  const log = e.target;
  state.monAutoScroll = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
});
$("btnSend").onclick = async () => {
  const input = $("monInput");
  const hex = input.value.trim();
  if (!hex) return;
  $("sendErr").classList.add("hidden");
  try {
    await api("/api/send", { hex });
    input.value = "";
  } catch (e) {
    $("sendErr").textContent = e.message;
    $("sendErr").classList.remove("hidden");
  }
};
$("monInput").addEventListener("keydown", (e) => { if (e.key === "Enter") $("btnSend").click(); });

$("btnAddCmd").onclick = () => { addCmdRow(); savePrefs(); };
$("btnSeqStart").onclick = startSequence;
$("btnSeqStop").onclick = async () => { await api("/api/sequence", { action: "stop" }); };
document.querySelectorAll('input[name="loopMode"]').forEach((r) => (r.onchange = savePrefs));
function openDialog(id) {
  const dlg = $(id);
  // 旧浏览器无 <dialog> 支持：无 showModal 时内容已随文档流可见，避免 JS 报错
  if (dlg && typeof dlg.showModal === "function") dlg.showModal();
}
// 标签页切回前台/窗口聚焦时，若断线则立即重连（后台期间定时器被浏览器节流，
// 仅靠 2s 定时重连可能长时间无进展）
function reconnectIfDown() {
  if (state.wsDown && (!state.ws || state.ws.readyState !== WebSocket.OPEN)) {
    connectWS();
  }
}
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") reconnectIfDown();
});
window.addEventListener("focus", reconnectIfDown);

$("btnAbout").onclick = () => openDialog("dlgAbout");

// 顶栏"一键重置"：清除本机保存的数据并重载，恢复首次打开的状态。
// 仅清除浏览器本地数据（不影响服务端与其他客户端，也不改变串口当前开关状态）；
// 注意：正在运行的指令序列在服务端，若需停止请先在④中使用"停止"。
$("btnResetLocal").onclick = () => {
  const ok = confirm(
    "确定要清除本机保存的设置并重置吗？\n\n" +
    "将清除：串口参数、路数、各通道接线视角、自定义指令、界面偏好、实例标识。\n" +
    "清除后页面将自动重新加载，恢复初始状态。\n\n" +
    "（不影响串口当前开关状态与其他客户端；如正在执行指令序列，请先停止）"
  );
  if (!ok) return;
  // 清除本应用在 localStorage 中的全部键（含历史版本遗留键），随后强制重载
  Object.keys(localStorage)
    .filter((k) => k.startsWith("chpilot.") || k.startsWith("webcom.") || k === "channelWiring" || k === "channelCount")
    .forEach((k) => localStorage.removeItem(k));
  location.reload();
};
$("btnHelp").onclick = () => openDialog("dlgHelp");
[$("baudRate"), $("dataBits"), $("stopBits"), $("parity"), $("flowControl"), $("selPort")].forEach((el) => (el.onchange = savePrefs));

/* ---------- 顶部操作提示 toast ---------- */
function showToast(text, isError) {
  const box = $("toastBox");
  const item = document.createElement("div");
  item.className = "toast" + (isError ? " error" : "");
  item.textContent = text;
  box.appendChild(item);
  // 触发入场动画
  requestAnimationFrame(() => item.classList.add("show"));
  setTimeout(() => {
    item.classList.remove("show");
    setTimeout(() => item.remove(), 400);
  }, 4000);
  // 最多同时 4 条，超出移除最旧的
  while (box.children.length > 4) box.removeChild(box.firstChild);
}

/* ---------- 启动 ---------- */
function updateClientLabel() {
  $("clientIdLabel").textContent = state.clientIp
    ? `${CLIENT_ID}@${state.clientIp}`
    : CLIENT_ID;
}
updateClientLabel();
loadPrefs();
refreshPorts();
// 先应用一次权威状态再接入 WS，避免加载期状态快照与首推并发导致的覆盖竞态
refreshStatus().finally(connectWS);
