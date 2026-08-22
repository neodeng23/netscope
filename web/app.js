// netscope 体检台前端（无构建链，原生 JS）
// 所有检测都由按钮触发：单通道检测 / 全节点对比 / 趋势图均一次点击一次执行。
const $ = (s) => document.querySelector(s);
const state = { targets: [], subs: [], nodes: [], groups: [], polling: null, subPolling: null, matrixSort: 'pass', lastJob: null };

// ---------- API ----------

async function api(path, opts) {
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  return r.json();
}

// ---------- 渲染 ----------

function renderTargets() {
  const filter = $('#group-filter').value;
  const tb = $('#targets tbody');
  tb.innerHTML = '';
  const list = state.targets.filter((t) => !filter || t.group === filter);
  $('#empty-tip').hidden = list.length > 0;
  for (const t of list) {
    const tr = document.createElement('tr');
    const group = `<span class="tag group-tag${t.group ? '' : ' muted'}" data-group="${t.id}" title="点击修改分组">${escapeHTML(t.group || '未分组')}</span>`;
    tr.innerHTML = `
      <td><input type="checkbox" class="pick" value="${t.id}" checked></td>
      <td class="wrap">${escapeHTML(t.url)}${t.note ? ' <span class="tag">' + escapeHTML(t.note) + '</span>' : ''}</td>
      <td>${group}</td>
      <td>${fmtTime(t.created)}</td>
      <td><button class="danger" data-del="${t.id}">删除</button></td>`;
    tb.appendChild(tr);
  }
}

function renderGroupFilter() {
  const sel = $('#group-filter');
  const cur = sel.value;
  sel.innerHTML = '<option value="">全部分组</option>';
  state.groups.forEach((g) => {
    const o = document.createElement('option');
    o.value = g;
    o.textContent = g;
    sel.appendChild(o);
  });
  if (cur) sel.value = cur;
}

function renderResult(item, withNode) {
  const tr = document.createElement('tr');
  const nodeCell = withNode ? `<td class="wrap">${escapeHTML(item.node || '-')}</td>` : '';
  if (!item.result) {
    tr.innerHTML = `<td class="wrap">${escapeHTML(item.url)}</td>${nodeCell}
      <td><span class="pill run">检测中…</span></td><td>-</td><td>-</td><td>-</td><td>-</td><td>-</td><td>-</td>`;
    return tr;
  }
  const r = item.result;
  let pill;
  if (r.ok) pill = '<span class="pill ok">✅ 可访问</span>';
  else if (r.reachable && r.status) pill = `<span class="pill warn">🟡 可达 · HTTP ${r.status}</span>`;
  else pill = '<span class="pill bad">❌ 不可达</span>';
  tr.innerHTML = `
    <td class="wrap">${escapeHTML(item.url)}</td>${nodeCell}
    <td>${pill}</td>
    <td>${r.status || '-'}</td>
    <td>${r.totalMs ? r.totalMs.toFixed(1) + 'ms' : '-'}</td>
    <td>${r.connMs ? r.connMs.toFixed(1) + 'ms' : '-'}</td>
    <td>${escapeHTML(r.exitIp || '-')}${r.via && r.via !== 'direct' ? '<span class="tag">' + escapeHTML(r.via) + '</span>' : ''}</td>
    <td>${escapeHTML(r.location || '-')}${r.ipFlags ? '<span class="tag">' + escapeHTML(r.ipFlags) + '</span>' : ''}</td>
    <td class="wrap" style="color:var(--sub)">${escapeHTML(r.err || '')}</td>`;
  return tr;
}

function isAllNodeJob(job) {
  return job.via && job.via.startsWith('全部节点');
}

function renderResults(job) {
  state.lastJob = job;
  if (isAllNodeJob(job)) { renderMatrix(job); return; }
  $('#matrix-box').hidden = true;
  $('#results').hidden = false;
  const withNode = job.items.some((it) => it.node);
  $('#th-node').hidden = !withNode;
  const tb = $('#results tbody');
  tb.innerHTML = '';
  $('#result-tip').hidden = true;
  for (const item of job.items) tb.appendChild(renderResult(item, withNode));
  const p = $('#progress');
  if (job.finished) {
    p.textContent = `完成：${job.done}/${job.total} · ${job.via}`;
    $('#btn-run').disabled = false;
    $('#btn-run-all').disabled = false;
    if (state.polling) { clearInterval(state.polling); state.polling = null; }
  } else {
    p.textContent = `检测中 ${job.done}/${job.total} · ${job.via}`;
  }
  renderResultNotes(job);
}


function renderSubs() {
  const tb = $('#subs tbody');
  tb.innerHTML = '';
  $('#subs-tip').hidden = state.subs.length > 0;
  const loading = state.subs.some((x) => !x.nodes && !x.err);
  $('#subs-progress').textContent = loading ? '加载中…' : '';
  for (const sub of state.subs) {
    const tr = document.createElement('tr');
    let count;
    if (sub.err) count = `<span class="pill bad" title="${escapeHTML(sub.err)}">失败</span>`;
    else if (sub.nodes > 0) count = `<span class="pill ok">${sub.nodes}</span>`;
    else count = '<span class="pill run">…</span>';
    tr.innerHTML = `
      <td class="sub-cell" title="${escapeHTML(sub.url)}">${escapeHTML(sub.url)}${sub.note ? '<span class="sub-note">' + escapeHTML(sub.note) + '</span>' : ''}</td>
      <td>${count}</td>
      <td>${fmtTime(sub.added)}</td>
      <td><button class="danger" data-sub-del="${sub.id}">删除</button></td>`;
    tb.appendChild(tr);
  }
}

function renderNodes() {
  const sel = $('#via');
  const cur = sel.value;
  sel.innerHTML = '<option value="direct">⏱ 直连（本机）</option>';
  state.nodes.forEach((n, i) => {
    const o = document.createElement('option');
    o.value = n.name;
    o.textContent = `${i + 1}. ${n.name}（${n.type}）`;
    sel.appendChild(o);
  });
  if (cur) sel.value = cur;
}

// ---------- 交互 ----------

async function refresh() {
  const s = await api('/api/state');
  state.targets = s.targets || [];
  state.subs = s.subs || [];
  state.groups = s.groups || [];
  state.nodes = s.nodes || [];
  renderSubs();
  renderGroupFilter();
  renderTargets();
  renderNodes();
}

async function addSub() {
  const input = $('#new-sub');
  const url = input.value.trim();
  if (!url) return;
  try {
    const { added } = await api('/api/subs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url }),
    });
    if (!added) { alert('该订阅已在清单中'); }
    input.value = '';
    await refresh();
    watchSubsLoading();
  } catch (e) { alert('添加失败：' + e.message); }
}

// 添加订阅后轮询,直到节点数出现或报错(最长 2 分钟)
function watchSubsLoading() {
  if (state.subPolling) clearInterval(state.subPolling);
  const deadline = Date.now() + 120 * 1000;
  state.subPolling = setInterval(async () => {
    try { await refresh(); } catch (e) { return; }
    const busy = state.subs.some((x) => !x.nodes && !x.err);
    if (!busy || Date.now() > deadline) {
      clearInterval(state.subPolling);
      state.subPolling = null;
    }
  }, 1200);
}

async function delSub(id) {
  await api('/api/subs/' + id, { method: 'DELETE' });
  await refresh();
  watchSubsLoading();
}

async function addTarget() {
  const input = $('#new-url');
  const url = input.value.trim();
  if (!url) return;
  try {
    await api('/api/targets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url, group: $('#new-group').value.trim() }),
    });
    input.value = '';
    await refresh();
  } catch (e) { alert('添加失败：' + e.message); }
}

async function editGroup(id) {
  const t = state.targets.find((x) => x.id === id);
  if (!t) return;
  const group = prompt('修改分组（留空为未分组）：', t.group || '');
  if (group === null) return;
  try {
    await api('/api/targets/update', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, group: group.trim() }),
    });
    await refresh();
  } catch (e) { alert('修改失败：' + e.message); }
}

async function delTarget(id) {
  await api('/api/targets/' + id, { method: 'DELETE' });
  await refresh();
}

async function importTargets(file) {
  try {
    const items = JSON.parse(await file.text());
    if (!Array.isArray(items)) throw new Error('导入内容需为 JSON 数组');
    const { added } = await api('/api/targets/import', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(items),
    });
    alert(`导入完成：新增 ${added} 条（重复已跳过）`);
    await refresh();
  } catch (e) { alert('导入失败：' + e.message); }
}

async function runChecks(via) {
  const ids = [...document.querySelectorAll('.pick:checked')].map((c) => c.value);
  if (ids.length === 0) { alert('请先勾选要检测的地址'); return; }
  $('#btn-run').disabled = true;
  $('#btn-run-all').disabled = true;
  $('#result-tip').hidden = true;
  $('#results tbody').innerHTML = '';
  try {
    const { id } = await api('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids, via }),
    });
    state.polling = setInterval(async () => {
      const job = await api('/api/jobs/' + id);
      renderResults(job);
    }, 700);
  } catch (e) {
    alert('发起检测失败：' + e.message);
    $('#btn-run').disabled = false;
    $('#btn-run-all').disabled = false;
  }
}

// ---------- 全节点对比矩阵 ----------

function cellTitle(r) {
  if (!r) return '等待检测';
  const parts = [`状态码: ${r.status || '-'}`, `总耗时: ${r.totalMs ? r.totalMs.toFixed(1) + 'ms' : '-'}`, `建连: ${r.connMs ? r.connMs.toFixed(1) + 'ms' : '-'}`];
  if (r.exitIp) parts.push(`出口: ${r.exitIp} ${r.location || ''}${r.ipFlags ? '（' + r.ipFlags + '）' : ''}`);
  if (r.err) parts.push(`错误: ${r.err}`);
  return parts.join('\n');
}

function matrixCell(r) {
  if (!r) return '<td class="m-run">…</td>';
  const tip = cellTitle(r).replace(/"/g, '&quot;');
  if (r.ok) return `<td class="m-ok" title="${tip}">✅ ${r.totalMs ? Math.round(r.totalMs) : ''}</td>`;
  if (r.reachable && r.status) return `<td class="m-warn" title="${tip}">🟡 ${r.status}</td>`;
  return `<td class="m-bad" title="${tip}">❌</td>`;
}

function renderMatrix(job) {
  $('#results').hidden = true;
  $('#result-tip').hidden = true;
  const box = $('#matrix-box');
  box.hidden = false;
  $('#sort-pass').classList.toggle('active', state.matrixSort === 'pass');
  $('#sort-latency').classList.toggle('active', state.matrixSort === 'latency');

  // 地址列(保序去重)与节点行
  const urls = [...new Set(job.items.map((it) => it.url))];
  const rows = new Map(); // node -> {cells: Map(url->result), done, ok, sumMs, n}
  for (const it of job.items) {
    if (!rows.has(it.node)) rows.set(it.node, { cells: new Map(), ok: 0, reachable: 0, sumMs: 0, n: 0, done: 0 });
    const row = rows.get(it.node);
    row.cells.set(it.url, it.result);
    if (it.result) {
      row.done++;
      if (it.result.ok) { row.ok++; row.sumMs += it.result.totalMs || 0; row.n++; }
      else if (it.result.reachable) row.reachable++;
    }
  }

  const nodeNames = [...rows.keys()];
  const sorter = {
    pass: (a, b) => {
      const ra = rows.get(a), rb = rows.get(b);
      return (rb.ok - ra.ok) || ((ra.n ? ra.sumMs / ra.n : 1e9) - (rb.n ? rb.sumMs / rb.n : 1e9));
    },
    latency: (a, b) => {
      const ra = rows.get(a), rb = rows.get(b);
      return (ra.n ? ra.sumMs / ra.n : 1e9) - (rb.n ? rb.sumMs / rb.n : 1e9);
    },
  };
  nodeNames.sort(sorter[state.matrixSort] || sorter.pass);

  const shortUrl = (u) => u.replace(/^https?:\/\//, '').replace(/\/$/, '');
  const head = urls.map((u) => `<th class="m-url" title="${escapeHTML(u)}">${escapeHTML(shortUrl(u))}</th>`).join('');
  const body = nodeNames.map((name) => {
    const row = rows.get(name);
    const avg = row.n ? (row.sumMs / row.n).toFixed(0) + 'ms' : '-';
    const rate = row.ok + row.reachable > 0 ? `${row.ok}/${urls.length}` : `${row.ok}/${urls.length}`;
    const grade = row.ok === urls.length ? 'row-full' : row.ok + row.reachable > 0 ? 'row-part' : 'row-none';
    const cells = urls.map((u) => matrixCell(row.cells.get(u))).join('');
    return `<tr class="${grade}"><td class="m-node" title="${escapeHTML(name)}">${escapeHTML(name)}</td>${cells}<td class="m-rate">${rate}</td><td class="m-avg">${avg}</td></tr>`;
  }).join('');

  $('#matrix').innerHTML = `
    <thead><tr><th class="m-node">节点</th>${head}<th>通过</th><th>平均延迟</th></tr></thead>
    <tbody>${body}</tbody>`;

  const done = job.items.filter((it) => it.result).length;
  const full = nodeNames.filter((n) => rows.get(n).ok === urls.length).length;
  $('#matrix-summary').textContent = `${nodeNames.length} 节点 × ${urls.length} 地址 · 已完成 ${done}/${job.items.length} · 全通过 ${full} 个`;
}

// ---------- 结果说明 ----------

// 常见异常状态码解释（仅对本次结果里出现过的展示）
const STATUS_EXPLAIN = {
  400: '请求错误：服务器认为请求本身有问题，普通 GET 检测很少见，多为站点接口限制',
  401: '未授权：需要登录/鉴权才能看，网络与出口都是通的',
  403: '被拒绝：最常见是 Cloudflare 等防护在拦截——出口 IP 被风控（机房/代理 IP 信誉差）或指纹不像真人浏览器。换一个节点通道常常就能过（不同出口 IP 信誉不同）',
  404: '页面不存在：域名和路径都通了，只是这个地址没有内容，不代表站点不可达',
  405: '方法不允许：站点拒绝 GET 请求，少见',
  408: '请求超时：服务器收到请求但处理太慢',
  418: "I'm a teapot：站点反爬彩蛋，网络是通的",
  421: '请求导向错误（Misdirected Request）：连接被复用/导向到了错误的服务节点，SNI 或 CDN 调度不匹配。OpenAI 等站点对「看起来不对」的来源也常回 421；换个节点通道或稍后重试一般可解',
  429: '请求过多：触发了速率限制，IP 被临时限流，过段时间或换通道再试',
  451: '因法律原因拒绝：地区性封锁（如 GDPR 或内容审查），该出口 IP 所在地区不被允许访问',
  500: '服务器内部错误：站点自己的问题，与你的链路无关',
  502: '网关错误：站点后端服务故障或网关异常',
  503: '服务不可用：站点过载/维护，部分是防护的人机验证页（浏览器打开可能正常）',
  504: '网关超时：站点后端响应超时',
  521: 'Cloudflare 521：源站拒绝连接，网站自己的服务器离线',
  522: 'Cloudflare 522：连接源站超时',
  523: 'Cloudflare 523：源站不可达',
  525: 'Cloudflare 525：与源站的 SSL 握手失败',
};

function explainStatus(code) {
  if (STATUS_EXPLAIN[code]) return STATUS_EXPLAIN[code];
  if (code >= 400 && code < 500) return '客户端侧错误（4xx）：网络是通的，请求被服务方拒绝';
  if (code >= 500) return '服务端错误（5xx）：站点自身异常，与链路无关';
  return '';
}

function renderResultNotes(job) {
  const box = $('#result-notes');
  const seen = new Set();
  for (const it of job.items) {
    const r = it.result;
    if (r && !r.ok && r.status) seen.add(r.status);
  }
  const codes = [...seen].sort((a, b) => a - b);
  if (codes.length === 0) { box.hidden = true; box.innerHTML = ''; return; }
  const lines = codes.map((c) => `<div class="note-line"><b>HTTP ${c}</b> — ${explainStatus(c)}</div>`);
  box.innerHTML = `
    <div class="note-line legend">图例：✅ 可访问（2xx/3xx） · 🟡 网络可达但被拒（内容层拦截） · ❌ 网络不可达（建连/超时失败）</div>
    ${lines.join('')}`;
  box.hidden = false;
}

// ---------- 本机网络体检 ----------

function siteRows(sites) {
  return (sites || []).map((x) => `
    <tr>
      <td>${escapeHTML(x.name)}</td>
      <td>${x.ok ? '<span class="pill ok">✅ ' + (x.status || '') + '</span>' : '<span class="pill bad">❌</span>'}</td>
      <td>${x.totalMs ? x.totalMs.toFixed(1) + 'ms' : '-'}</td>
    </tr>`).join('');
}

function pingLine(p) {
  if (!p || !p.recv) return '❌ 不通';
  return `avg ${p.avgMs.toFixed(1)}ms · 抖动 ${p.jitterMs.toFixed(1)}ms · 丢包 ${p.lossPct.toFixed(0)}%`;
}

function perspectivePanel(title, icon, p, extraRows) {
  const head = extraRows.map(([k, v]) => `<div class="kv"><span>${k}</span><b>${v}</b></div>`).join('');
  return `
  <div class="local-col">
    <h3>${icon} ${title}</h3>
    ${p.err && !p.ip ? `<div class="kv"><span>查询失败</span><b>${escapeHTML(p.err)}</b></div>` : ''}
    <div class="kv"><span>出口 IP</span><b>${escapeHTML(p.ip || '-')}</b></div>
    <div class="kv"><span>归属地</span><b>${escapeHTML(p.location || '-')}</b></div>
    ${head}
    <div class="kv"><span>基准延迟</span><b>${pingLine(p.ping)}</b></div>
    <table class="grid"><thead><tr><th>站点</th><th>状态</th><th>耗时</th></tr></thead><tbody>${siteRows(p.sites)}</tbody></table>
  </div>`;
}

function renderLocal(r) {
  const box = $('#local-box');
  const fExtra = [];
  if (r.foreign) {
    if (r.foreign.isp) fExtra.push(['ISP', escapeHTML(r.foreign.isp)]);
    if (r.foreign.flags) fExtra.push(['IP 标记', escapeHTML(r.foreign.flags)]);
    if (r.foreign.risk >= 0) fExtra.push(['风险分', r.foreign.risk + '/100']);
  }
  let dnsLine = '未检测';
  if (r.dns && r.dns.detail) {
    const tag = r.dns.polluted === true ? '<span class="pill bad">疑似污染</span>'
      : r.dns.polluted === false ? '<span class="pill ok">正常</span>' : '<span class="pill run">无法判定</span>';
    dnsLine = `${tag} ${escapeHTML(r.dns.detail)}`;
  }
  let exitLine = '未知';
  if (r.sameExit !== null && r.sameExit !== undefined) {
    exitLine = r.sameExit
      ? '<span class="pill ok">一致</span> 国内外看到的出口相同（无分流）'
      : '<span class="pill bad">不一致</span> 国内外看到的出口不同（存在代理/分流/TUN）';
  }
  box.innerHTML = `
    <div class="kv-line">体检时间 ${new Date(r.time).toLocaleString()}</div>
    <div class="local-grid">
      ${perspectivePanel('从国内测试', '🇨🇳', r.domestic || {}, [])}
      ${perspectivePanel('从国外测试', '🌐', r.foreign || {}, fExtra)}
    </div>
    <div class="kv-line">DNS：${dnsLine}</div>
    <div class="kv-line">出口一致性：${exitLine}</div>`;
}

async function runLocal() {
  const btn = $('#btn-local');
  btn.disabled = true;
  $('#local-progress').textContent = '体检中…（约 5~15 秒）';
  try {
    const r = await fetch('/api/local', { method: 'POST' });
    if (!r.ok) throw new Error(await r.text());
    renderLocal(await r.json());
    $('#local-progress').textContent = '完成';
  } catch (e) {
    $('#local-progress').textContent = '';
    alert('体检失败：' + e.message);
  } finally {
    btn.disabled = false;
  }
}

// ---------- 工具 ----------

function escapeHTML(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}
function fmtTime(iso) {
  if (!iso) return '-';
  const d = new Date(iso);
  return isNaN(d) ? '-' : `${d.getMonth() + 1}-${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}
function fmtSize(n) {
  if (!n && n !== 0) return '-';
  if (n > 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n > 1024) return (n / 1024).toFixed(0) + ' KB';
  return n + ' B';
}

// ---------- 启动 ----------

// 重新拉取全部订阅
async function reloadSubs() {
  const btn = $('#btn-sub-reload');
  btn.disabled = true;
  try {
    await api('/api/subs/reload', { method: 'POST' });
    $('#subs-progress').textContent = '重新拉取中…';
    await refresh();
    watchSubsLoading();
  } catch (e) {
    alert('重新拉取失败：' + e.message);
  } finally {
    btn.disabled = false;
  }
}

$('#btn-sub-reload').addEventListener('click', reloadSubs);
$('#btn-sub-add').addEventListener('click', addSub);
$('#new-sub').addEventListener('keydown', (e) => { if (e.key === 'Enter') addSub(); });
$('#subs tbody').addEventListener('click', (e) => {
  const id = e.target.dataset && e.target.dataset.subDel;
  if (id) delSub(id);
});
$('#btn-add').addEventListener('click', addTarget);
$('#new-url').addEventListener('keydown', (e) => { if (e.key === 'Enter') addTarget(); });
$('#btn-run').addEventListener('click', () => runChecks($('#via').value));
$('#btn-run-all').addEventListener('click', () => runChecks('all'));
$('#btn-refresh').addEventListener('click', async () => {
  await refresh();
});
$('#btn-export').addEventListener('click', () => { location.href = '/api/targets/export'; });
$('#file-import').addEventListener('change', (e) => {
  if (e.target.files.length > 0) importTargets(e.target.files[0]);
  e.target.value = '';
});
$('#check-all').addEventListener('change', (e) => {
  document.querySelectorAll('.pick').forEach((c) => (c.checked = e.target.checked));
});
$('#group-filter').addEventListener('change', renderTargets);
$('#targets tbody').addEventListener('click', (e) => {
  const t = e.target;
  if (t.dataset && t.dataset.del) { delTarget(t.dataset.del); return; }
  if (t.dataset && t.dataset.group) { editGroup(t.dataset.group); }
});
refresh().catch((e) => {
  $('#result-tip').textContent = '加载失败：' + e.message + '（如配置了 --token，需在 URL 加 ?token=xxx）';
});
$('#btn-local').addEventListener('click', runLocal);
$('#sort-pass').addEventListener('click', () => { state.matrixSort = 'pass'; rerenderMatrix(); });
$('#sort-latency').addEventListener('click', () => { state.matrixSort = 'latency'; rerenderMatrix(); });
function rerenderMatrix() {
  if (state.lastJob && isAllNodeJob(state.lastJob)) renderMatrix(state.lastJob);
}
