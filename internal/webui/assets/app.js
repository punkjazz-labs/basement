const state = { csrf: '', recipes: [], models: [], jobs: [] };
const $ = selector => document.querySelector(selector);
const terminal = value => ['ready', 'failed', 'cancelled', 'stopped', 'removed'].includes(value);
const formatBytes = value => {
  if (!value) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) { value /= 1000; unit += 1; }
  return `${value.toFixed(unit > 2 ? 1 : 0)} ${units[unit]}`;
};
const escapeHTML = value => { const node = document.createElement('div'); node.textContent = String(value ?? ''); return node.innerHTML; };

async function api(path, options = {}) {
  options.headers = { ...(options.headers || {}) };
  if (options.method && options.method !== 'GET') {
    options.headers['X-CSRF-Token'] = state.csrf;
    options.headers['Content-Type'] = 'application/json';
  }
  const response = await fetch(path, options);
  const body = await response.json().catch(() => ({ error: response.statusText }));
  if (!response.ok) throw new Error(body.error || response.statusText);
  return body;
}

async function boot() {
  try {
    const status = await api('/api/v1/auth/status');
    if (!status.authenticated) return showPairing();
    state.csrf = status.csrf_token;
    $('#pairing').classList.add('hidden');
    $('#console').classList.remove('hidden');
    await refresh();
    setInterval(refreshJobs, 2000);
  } catch (_) { showPairing(); }
}

function showPairing() {
  $('#pairing').classList.remove('hidden');
  $('#console').classList.add('hidden');
  $('#connection').textContent = 'Pairing required';
}

$('#pair-form').addEventListener('submit', async event => {
  event.preventDefault();
  $('#pair-error').textContent = '';
  try {
    const result = await api('/api/v1/auth/pair', { method: 'POST', body: JSON.stringify({ token: $('#pair-token').value }) });
    state.csrf = result.csrf_token;
    await boot();
  } catch (error) { $('#pair-error').textContent = error.message; }
});
$('#refresh').addEventListener('click', refresh);

async function refresh() {
  try {
    const [system, recipes, models, jobs] = await Promise.all([
      api('/api/v1/system'), api('/api/v1/recipes'), api('/api/v1/models'), api('/api/v1/jobs')
    ]);
    Object.assign(state, { recipes, models, jobs });
    renderSystem(system); renderRecipes(); renderJobs();
    $('#connection').textContent = 'Connected'; $('#connection').className = 'status good';
  } catch (_) { $('#connection').textContent = 'Disconnected'; $('#connection').className = 'status bad'; }
}

async function refreshJobs() {
  try {
    state.jobs = await api('/api/v1/jobs'); state.models = await api('/api/v1/models');
    renderJobs(); renderRecipes();
  } catch (_) { /* the connection badge is updated by the next full refresh */ }
}

function renderSystem(system) {
  $('#hostname').textContent = system.hostname || 'DGX Spark';
  $('#system-summary').textContent = `${system.product_name || 'Unknown hardware'} · ${system.architecture} · ${system.os} · Manager ${system.manager_version}`;
  $('#storage').textContent = formatBytes(system.storage_available_bytes);
  $('#recipe-count').textContent = `${state.recipes.length} candidate${state.recipes.length === 1 ? '' : 's'}`;
  const blockers = system.blocking_conditions || [];
  $('#blockers').classList.toggle('hidden', !blockers.length);
  $('#blocker-list').innerHTML = blockers.map(value => `<li>${escapeHTML(value)}</li>`).join('');
}

function renderRecipes() {
  const installed = new Map(state.models.map(model => [model.recipe_id, model]));
  const order = ['qwen36-35b-a3b-nvfp4-1s', 'qwen36-27b-nvfp4-1s', 'laguna-s-2-1-nvfp4-dflash-1s'];
  const recipes = [...state.recipes].sort((a, b) => order.indexOf(a.id) - order.indexOf(b.id));
  $('#recipes').innerHTML = recipes.map((item, index) => {
    const model = installed.get(item.id);
    const busy = state.jobs.some(job => job.recipe_id === item.id && !terminal(job.state));
    const featured = index === 0;
    let actions;
    if (!model) actions = `<button data-action="install" data-id="${item.id}" ${busy ? 'disabled' : ''}>Install</button>`;
    else if (model.active && model.status === 'ready') actions = `<button data-action="stop" data-id="${item.id}" ${busy ? 'disabled' : ''}>Stop</button><button class="secondary" data-action="smoke-test" data-id="${item.id}">Smoke test</button><button class="secondary" data-action="copy-endpoint" data-id="${item.id}">Copy endpoint</button><button class="secondary" data-action="copy-model" data-id="${item.id}">Copy model ID</button>`;
    else if (model.status === 'recovering') actions = '<button disabled>Recovering health…</button>';
    else actions = `<button data-action="start" data-id="${item.id}" ${busy ? 'disabled' : ''}>Start</button><button class="danger" data-action="remove" data-id="${item.id}">Remove</button>`;
    const artifacts = item.artifacts.map(artifact => `<li><span>${escapeHTML(artifact.role)}</span><div><strong>${escapeHTML(artifact.repository)}</strong><code>${artifact.revision.slice(0, 12)} · ${formatBytes(artifact.expected_bytes)}</code></div></li>`).join('');
    const speculation = item.service.vllm.speculative_method === 'dflash' ? `DFlash ×${item.service.vllm.speculative_tokens}` : `MTP ×${item.service.vllm.speculative_tokens}`;
    const backend = item.service.vllm.linear_backend || item.service.vllm.moe_backend || 'automatic';
    return `<article class="card ${featured ? 'featured' : ''}">
      <div class="card-index">${String(index + 1).padStart(2, '0')}</div>
      <div class="card-head"><div><p class="eyebrow">${escapeHTML(item.publisher)}</p><h3>${escapeHTML(item.display_name)}</h3><p class="model-id">${escapeHTML(item.service.served_model_id)}</p></div><div class="badges">${featured ? '<span class="pill recommended">recommended</span>' : ''}<span class="pill">${escapeHTML(item.verification)}</span></div></div>
      <div class="signal"><span>${escapeHTML(speculation)}</span><span>${formatBytes(item.artifact_bytes)} weights</span><span>${item.artifacts.length} artifact${item.artifacts.length === 1 ? '' : 's'}</span></div>
      <div class="facts"><div><span>RUNTIME</span><strong>vLLM · ${escapeHTML(backend)}</strong></div><div><span>REQUIRED DISK</span><strong>${formatBytes(item.required_bytes)}</strong></div><div><span>ENDPOINT</span><strong>:${item.service.default_host_port}/v1</strong></div><div><span>STATE</span><strong class="model-state">${model ? escapeHTML(model.status) : 'Not installed'}</strong></div></div>
      <details><summary>Pins & provenance</summary><ul class="artifacts">${artifacts}</ul><a class="source" href="${escapeHTML(item.source.url)}" target="_blank" rel="noreferrer">Upstream source @ ${item.source.revision.slice(0, 12)} ↗</a><code class="digest">${escapeHTML(item.runtime.digest)}</code></details>
      <div class="actions">${actions}</div>
    </article>`;
  }).join('');
  document.querySelectorAll('[data-action]').forEach(button => { button.onclick = () => action(button.dataset.action, button.dataset.id); });
}

function renderJobs() {
  if (!state.jobs.length) { $('#jobs').innerHTML = '<p class="muted">No jobs yet.</p>'; return; }
  $('#jobs').innerHTML = state.jobs.map(job => {
    const step = [...job.steps].reverse().find(item => item.receipt && item.receipt.percent);
    const percent = step ? step.receipt.percent : terminal(job.state) ? 100 : 5;
    return `<article class="job"><span class="status ${job.state === 'failed' ? 'bad' : terminal(job.state) ? 'good' : 'neutral'}">${escapeHTML(job.state)}</span><div><strong>${escapeHTML(job.kind)} · ${escapeHTML(job.recipe_id)}</strong><div class="progress"><i style="width:${Math.min(100, percent)}%"></i></div>${job.error ? `<p class="error">${escapeHTML(job.error)}</p>` : ''}</div><code>${job.id.slice(0, 12)}</code></article>`;
  }).join('');
}

async function action(name, id) {
  try {
    if (name === 'install') return await confirmInstall(id);
    const item = state.recipes.find(recipe => recipe.id === id);
    if (name === 'copy-endpoint') { await navigator.clipboard.writeText(`http://${location.hostname}:${item.service.default_host_port}/v1`); return; }
    if (name === 'copy-model') { await navigator.clipboard.writeText(item.service.served_model_id); return; }
    let options = { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: '{}' };
    if (name === 'remove') {
      if (!confirm('Remove this model runtime and generated configuration?')) return;
      const removeArtifacts = confirm(`Also remove ${formatBytes(item.artifact_bytes)} of owned model data? Cancel retains the downloaded artifact.`);
      options = { method: 'DELETE', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ remove_artifacts: removeArtifacts, expected_reclaim_bytes: removeArtifacts ? item.artifact_bytes : 0 }) };
      await api(`/api/v1/models/${id}`, options);
    } else await api(`/api/v1/models/${id}/${name}`, options);
    await refreshJobs();
  } catch (error) { alert(error.message); }
}

async function confirmInstall(id) {
  const item = state.recipes.find(recipe => recipe.id === id);
  const preflight = await api(`/api/v1/preflight?recipe_id=${encodeURIComponent(id)}`);
  if (!preflight.ready) {
    const blockers = preflight.checks.filter(check => !check.ok).map(check => check.error);
    for (const [name, present] of Object.entries(preflight.secrets)) if (!present) blockers.push(`${name} is missing`);
    alert(`Preflight failed without changing Docker or model data:\n\n${blockers.join('\n')}`);
    return;
  }
  $('#confirm-title').textContent = item.display_name;
  $('#confirm-detail').innerHTML = `<p><strong>Artifact</strong><br><code>${escapeHTML(item.artifacts[0].repository)}@${escapeHTML(item.artifacts[0].revision)}</code></p><p><strong>Required space</strong><br>${formatBytes(item.required_bytes)} including safety margin</p><p><strong>Endpoint</strong><br>Port ${item.service.default_host_port}</p><p><a href="${item.artifacts[0].licence_url}" target="_blank" rel="noreferrer">Read ${escapeHTML(item.artifacts[0].licence)} licence</a></p>`;
  $('#licence').checked = false; $('#confirm-dialog').showModal();
  $('#confirm-install').onclick = async event => {
    event.preventDefault();
    if (!$('#licence').checked) return alert('Licence acceptance is required.');
    try {
      await api(`/api/v1/models/${id}/install`, { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ confirmed: true, accept_licence: true }) });
      $('#confirm-dialog').close(); await refreshJobs();
    } catch (error) { alert(error.message); }
  };
}

boot();
