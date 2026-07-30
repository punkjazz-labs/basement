const setupNames = ['one', 'two', 'three', 'four'];
const requestedSetup = new URLSearchParams(location.search).get('sparks');
const state = {
  csrf: '', recipes: [], models: [], jobs: [],
  sparkCount: Math.max(1, setupNames.indexOf(requestedSetup) + 1),
  catalogueFingerprint: '', jobFingerprint: '', streams: new Map()
};
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
const productCopy = {
  'qwen36-35b-a3b-nvfp4-1s': { name: 'Qwen 3.6 35B', mark: 'Q35', verdict: 'Fast enough to become your default.', pace: '80 tok/s', use: 'Best all-rounder' },
  'qwen36-27b-nvfp4-1s': { name: 'Qwen 3.6 27B', mark: 'Q27', verdict: 'Flagship-level coding in a smaller footprint.', pace: '33 tok/s', use: 'Coding' },
  'laguna-s-2-1-nvfp4-dflash-1s': { name: 'Laguna S 2.1', mark: 'LS', verdict: 'Built for long, independent agent runs.', pace: '19.4 tok/s', use: 'Agentic work' }
};

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
    selectHardware(state.sparkCount, false);
    await refresh();
  } catch (_) { showPairing(); }
}

function showPairing() {
  $('#pairing').classList.remove('hidden');
  $('#console').classList.add('hidden');
  $('#connection').textContent = 'Pairing required';
  $('#connection').className = 'connection neutral';
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

document.querySelectorAll('[data-sparks]').forEach(button => {
  button.addEventListener('click', () => selectHardware(Number(button.dataset.sparks), true));
});
$('#refresh').addEventListener('click', refresh);

function selectHardware(count, updateURL) {
  state.sparkCount = count;
  document.querySelectorAll('[data-sparks]').forEach(button => {
    const selected = Number(button.dataset.sparks) === count;
    button.classList.toggle('selected', selected);
    button.setAttribute('aria-checked', String(selected));
  });
  if (updateURL) {
    const url = new URL(location.href);
    url.searchParams.set('sparks', setupNames[count - 1]);
    history.replaceState({}, '', url);
  }
  state.catalogueFingerprint = '';
  renderRecipes();
}

async function refresh() {
  try {
    const [system, recipes, models, jobs] = await Promise.all([
      api('/api/v1/system'), api('/api/v1/recipes'), api('/api/v1/models'), api('/api/v1/jobs')
    ]);
    Object.assign(state, { recipes, models, jobs });
    renderSystem(system);
    renderRecipes();
    renderJobs();
    syncJobStreams();
    $('#connection').textContent = 'Connected';
    $('#connection').className = 'connection good';
  } catch (_) {
    $('#connection').textContent = 'Disconnected';
    $('#connection').className = 'connection bad';
  }
}

async function refreshModelsAndJobs() {
  try {
    const [models, jobs] = await Promise.all([api('/api/v1/models'), api('/api/v1/jobs')]);
    Object.assign(state, { models, jobs });
    renderRecipes();
    renderJobs();
    syncJobStreams();
  } catch (_) { /* Manual refresh remains available if a stream is interrupted. */ }
}

function syncJobStreams() {
  const active = new Set(state.jobs.filter(job => !terminal(job.state)).map(job => job.id));
  for (const [id, stream] of state.streams) {
    if (!active.has(id)) { stream.close(); state.streams.delete(id); }
  }
  for (const id of active) {
    if (state.streams.has(id)) continue;
    const stream = new EventSource(`/api/v1/jobs/${encodeURIComponent(id)}/events`);
    state.streams.set(id, stream);
    stream.addEventListener('job', async event => {
      const job = JSON.parse(event.data);
      const index = state.jobs.findIndex(item => item.id === job.id);
      if (index === -1) state.jobs.unshift(job); else state.jobs[index] = job;
      renderJobs();
      renderRecipes();
      if (terminal(job.state)) {
        stream.close();
        state.streams.delete(job.id);
        await refreshModelsAndJobs();
      }
    });
  }
}

function renderSystem(system) {
  $('#hostname').textContent = system.hostname || 'DGX Spark';
  $('#system-summary').textContent = `${system.product_name || 'Unknown hardware'} · ${system.architecture}`;
  $('#storage').textContent = formatBytes(system.storage_available_bytes);
  const blockers = system.blocking_conditions || [];
  $('#blockers').classList.toggle('hidden', !blockers.length);
  $('#blocker-list').innerHTML = blockers.map(value => `<li>${escapeHTML(value)}</li>`).join('');
}

function catalogueState() {
  const models = state.models.map(({ recipe_id, status, active }) => [recipe_id, status, active]);
  const busy = state.jobs.filter(job => !terminal(job.state)).map(job => job.recipe_id).sort();
  return JSON.stringify([state.sparkCount, state.recipes.map(item => [item.id, item.version]), models, busy]);
}

function renderRecipes() {
  const fingerprint = catalogueState();
  if (fingerprint === state.catalogueFingerprint) return;
  state.catalogueFingerprint = fingerprint;
  const recipesElement = $('#recipes');
  const count = state.sparkCount === 1 ? state.recipes.length : 0;
  $('#recipe-count').textContent = String(count);
  $('#catalog-note').textContent = `For ${state.sparkCount} Spark${state.sparkCount === 1 ? '' : 's'}`;

  if (state.sparkCount !== 1) {
    recipesElement.innerHTML = `<div class="no-results"><strong>No ${state.sparkCount}-Spark recipes yet.</strong><p>Multi-Spark installs are next. Choose 1 Spark to install today.</p><button type="button" data-select-one>Show 1-Spark models</button></div>`;
    $('[data-select-one]').onclick = () => selectHardware(1, true);
    return;
  }

  const installed = new Map(state.models.map(model => [model.recipe_id, model]));
  const order = ['qwen36-35b-a3b-nvfp4-1s', 'qwen36-27b-nvfp4-1s', 'laguna-s-2-1-nvfp4-dflash-1s'];
  const recipes = [...state.recipes].sort((a, b) => order.indexOf(a.id) - order.indexOf(b.id));
  recipesElement.innerHTML = recipes.map(item => {
    const model = installed.get(item.id);
    const busy = state.jobs.some(job => job.recipe_id === item.id && !terminal(job.state));
    const copy = productCopy[item.id] || { name: item.display_name, mark: 'AI', verdict: 'Ready for your Spark.', pace: '—', use: 'Local model' };
    const status = busy ? 'Installing' : model ? model.status : 'Not installed';
    let primaryAction;
    let utilityActions = '';
    if (!model) primaryAction = `<button data-action="install" data-id="${item.id}" ${busy ? 'disabled' : ''}>${busy ? 'Installing' : 'Install'}</button>`;
    else if (model.active && model.status === 'ready') {
      primaryAction = `<button class="secondary" data-action="stop" data-id="${item.id}" ${busy ? 'disabled' : ''}>Stop</button>`;
      utilityActions = `<button class="secondary" data-action="smoke-test" data-id="${item.id}">Test</button><button class="secondary" data-action="copy-endpoint" data-id="${item.id}">Copy endpoint</button><button class="secondary" data-action="copy-model" data-id="${item.id}">Copy model ID</button>`;
    } else if (model.status === 'recovering') primaryAction = '<button disabled>Recovering</button>';
    else {
      primaryAction = `<button data-action="start" data-id="${item.id}" ${busy ? 'disabled' : ''}>Start</button>`;
      utilityActions = `<button class="danger" data-action="remove" data-id="${item.id}">Remove</button>`;
    }
    const artifacts = item.artifacts.map(artifact => `<li><span>${escapeHTML(artifact.role)}</span><code>${escapeHTML(artifact.repository)}@${artifact.revision.slice(0, 12)}</code></li>`).join('');
    return `<article class="model-row">
      <div class="model-row-main">
        <div class="model-mark" aria-hidden="true">${escapeHTML(copy.mark)}</div>
        <div class="model-copy"><div class="model-title"><h3>${escapeHTML(copy.name)}</h3>${item.id === order[0] ? '<span>Recommended</span>' : ''}</div><p>${escapeHTML(copy.verdict)}</p></div>
        <dl class="model-facts"><div><dt>Best for</dt><dd>${escapeHTML(copy.use)}</dd></div><div><dt>Speed</dt><dd>${escapeHTML(copy.pace)}</dd></div><div><dt>Disk</dt><dd>${formatBytes(item.required_bytes)}</dd></div></dl>
        <div class="model-status"><i class="${model && model.active ? 'active' : ''}"></i><span>${escapeHTML(status)}</span></div>
        <div class="primary-action">${primaryAction}</div>
        <details class="model-details"><summary aria-label="More about ${escapeHTML(item.display_name)}">•••</summary><div class="details-panel"><div><span>Model ID</span><code>${escapeHTML(item.service.served_model_id)}</code></div><div><span>Endpoint</span><code>:${item.service.default_host_port}/v1</code></div><div><span>Source</span><a href="${escapeHTML(item.source.url)}" target="_blank" rel="noreferrer">${escapeHTML(item.publisher)} ↗</a></div><ul>${artifacts}</ul><div class="utility-actions">${utilityActions}</div></div></details>
      </div>
    </article>`;
  }).join('');
  document.querySelectorAll('[data-action]').forEach(button => { button.onclick = () => action(button.dataset.action, button.dataset.id); });
}

function renderJobs() {
  const fingerprint = JSON.stringify(state.jobs);
  if (fingerprint === state.jobFingerprint) return;
  state.jobFingerprint = fingerprint;
  $('#job-count').textContent = String(state.jobs.length);
  if (!state.jobs.length) { $('#jobs').innerHTML = '<p class="empty-copy">Nothing running.</p>'; return; }
  $('#jobs').innerHTML = state.jobs.map(job => {
    const step = [...job.steps].reverse().find(item => item.receipt && item.receipt.percent);
    const percent = step ? step.receipt.percent : terminal(job.state) ? 100 : 5;
    return `<article class="job"><div><strong>${escapeHTML(job.kind)}</strong><span>${escapeHTML(job.recipe_id)}</span></div><div class="progress"><i style="width:${Math.min(100, percent)}%"></i></div><span class="job-state ${job.state === 'failed' ? 'failed' : ''}">${escapeHTML(job.state)}</span>${job.error ? `<p class="error">${escapeHTML(job.error)}</p>` : ''}</article>`;
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
      if (!confirm('Remove this model runtime and configuration?')) return;
      const removeArtifacts = confirm(`Also remove ${formatBytes(item.artifact_bytes)} of model data? Cancel keeps the download.`);
      options = { method: 'DELETE', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ remove_artifacts: removeArtifacts, expected_reclaim_bytes: removeArtifacts ? item.artifact_bytes : 0 }) };
      await api(`/api/v1/models/${id}`, options);
    } else await api(`/api/v1/models/${id}/${name}`, options);
    await refreshModelsAndJobs();
  } catch (error) { alert(error.message); }
}

async function confirmInstall(id) {
  const item = state.recipes.find(recipe => recipe.id === id);
  const preflight = await api(`/api/v1/preflight?recipe_id=${encodeURIComponent(id)}`);
  if (!preflight.ready) {
    const blockers = preflight.checks.filter(check => !check.ok).map(check => check.error);
    for (const [name, present] of Object.entries(preflight.secrets)) if (!present) blockers.push(`${name} is missing`);
    alert(`Setup needed:\n\n${blockers.join('\n')}`);
    return;
  }
  $('#confirm-title').textContent = item.display_name;
  $('#confirm-detail').innerHTML = `<dl class="install-facts"><div><dt>Download</dt><dd>${formatBytes(item.artifact_bytes)}</dd></div><div><dt>Space needed</dt><dd>${formatBytes(item.required_bytes)}</dd></div><div><dt>Port</dt><dd>${item.service.default_host_port}</dd></div></dl><a href="${item.artifacts[0].licence_url}" target="_blank" rel="noreferrer">Read the ${escapeHTML(item.artifacts[0].licence)} licence ↗</a>`;
  $('#licence').checked = false;
  $('#confirm-dialog').showModal();
  $('#confirm-install').onclick = async event => {
    event.preventDefault();
    if (!$('#licence').checked) return alert('Accept the licence to continue.');
    try {
      await api(`/api/v1/models/${id}/install`, { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ confirmed: true, accept_licence: true }) });
      $('#confirm-dialog').close();
      await refreshModelsAndJobs();
    } catch (error) { alert(error.message); }
  };
}

boot();
