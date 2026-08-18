// netscope 体检台前端（无构建链，原生 JS）
const $ = (s) => document.querySelector(s);
const state = { targets: [], nodes: [], polling: null };

// ---------- API ----------

async function api(path, opts) {
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  return r.json();
}

// ---------- 渲染 ----------

function renderTargets() {
  const tb = $('#targets tbody');
  tb.innerHTML = '';
  $('#empty-tip').hidden = state.targets.length > 0;
  for (const t of state.targets) {
    const tr = document.createElement('tr');
    tr.innerHTML = `
      <td><input type="checkbox" class="pick" value="${t.id}" checked></td>
      <td class="wrap">${escapeHTML(t.url)}${t.note ? ' <span class="tag">' + escapeHTML(t.note) + '</span>' : ''}</td>
      <td>${fmtTime(t.created)}</td>
      <td><button class="danger" data-del="${t.id}">删除</button></td>`;
    tb.appendChild(tr);
  }
}

function renderResult(item) {
  const tr = document.createElement('tr');
  if (!item.result) {
    tr.innerHTML = `<td class="wrap">${escapeHTML(item.url)}</td>
      <td><span class="pill run">检测中…</span></td><td>-</td><td>-</td><td>-</td><td>-</td><td>-</td><td>-</td>`;
    return tr;
  }
  const r = item.result;
  const ok = r.ok;
  tr.innerHTML = `
    <td class="wrap">${escapeHTML(item.url)}</td>
    <td>${ok ? '<span class="pill ok">✅ 可访问</span>' : '<span class="pill bad">❌ 不可达</span>'}</td>
    <td>${ok ? r.status : '-'}</td>
    <td>${r.totalMs ? r.totalMs.toFixed(1) + 'ms' : '-'}</td>
    <td>${r.connMs ? r.connMs.toFixed(1) + 'ms' : '-'}</td>
    <td>${escapeHTML(r.exitIp || '-')}${r.via && r.via !== 'direct' ? '<span class="tag">' + escapeHTML(r.via) + '</span>' : ''}</td>
    <td>${escapeHTML(r.location || '-')}${r.ipFlags ? '<span class="tag">' + escapeHTML(r.ipFlags) + '</span>' : ''}</td>
    <td class="wrap" style="color:var(--sub)">${escapeHTML(r.err || '')}</td>`;
  return tr;
}

function renderResults(job) {
  const tb = $('#results tbody');
  tb.innerHTML = '';
  $('#result-tip').hidden = true;
  for (const item of job.items) tb.appendChild(renderResult(item));
  const p = $('#progress');
  if (job.finished) {
    p.textContent = `完成：${job.done}/${job.total} · 通道 ${job.via}`;
    $('#btn-run').disabled = false;
    if (state.polling) { clearInterval(state.polling); state.polling = null; }
  } else {
    p.textContent = `检测中 ${job.done}/${job.total} · 通道 ${job.via}`;
  }
}

function renderReports(reports) {
  const tb = $('#reports tbody');
  tb.innerHTML = '';
  $('#report-tip').hidden = reports.length > 0;
  for (const r of reports) {
    const tr = document.createElement('tr');
    const isHtml = r.name.endsWith('.html');
    tr.innerHTML = `
      <td class="wrap">${r.name}</td><td>${r.mtime}</td><td>${fmtSize(r.size)}</td>
      <td>${isHtml ? `<a href="/reports/${encodeURIComponent(r.name)}" target="_blank">打开</a>` : '-'}</td>`;
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
  state.nodes = s.nodes || [];
  renderTargets();
  renderNodes();
  renderReports(s.reports || []);
}

async function addTarget() {
  const input = $('#new-url');
  const url = input.value.trim();
  if (!url) return;
  try {
    await api('/api/targets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url }),
    });
    input.value = '';
    await refresh();
  } catch (e) { alert('添加失败：' + e.message); }
}

async function delTarget(id) {
  await api('/api/targets/' + id, { method: 'DELETE' });
  await refresh();
}

async function runChecks() {
  const ids = [...document.querySelectorAll('.pick:checked')].map((c) => c.value);
  if (ids.length === 0) { alert('请先勾选要检测的地址'); return; }
  $('#btn-run').disabled = true;
  $('#result-tip').hidden = true;
  $('#results tbody').innerHTML = '';
  try {
    const { id } = await api('/api/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids, via: $('#via').value }),
    });
    state.polling = setInterval(async () => {
      const job = await api('/api/jobs/' + id);
      renderResults(job);
    }, 700);
  } catch (e) {
    alert('发起检测失败：' + e.message);
    $('#btn-run').disabled = false;
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

$('#btn-add').addEventListener('click', addTarget);
$('#new-url').addEventListener('keydown', (e) => { if (e.key === 'Enter') addTarget(); });
$('#btn-run').addEventListener('click', runChecks);
$('#check-all').addEventListener('change', (e) => {
  document.querySelectorAll('.pick').forEach((c) => (c.checked = e.target.checked));
});
$('#targets tbody').addEventListener('click', (e) => {
  const id = e.target.dataset && e.target.dataset.del;
  if (id) delTarget(id);
});
refresh().catch((e) => {
  $('#result-tip').textContent = '加载失败：' + e.message + '（如配置了 --token，需在 URL 加 ?token=xxx）';
});
