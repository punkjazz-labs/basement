const state = {
  csrf: '', system: null, recipes: [], models: [], jobs: [], selectedJobID: '',
  catalogueFingerprint: '', jobFingerprint: '', streams: new Map(), pendingModels: new Set()
};
const $ = selector => document.querySelector(selector);
const terminal = value => ['ready', 'failed', 'cancelled', 'stopped', 'removed'].includes(value);
const formatBytes = value => {
  if (!value) return 'Unknown';
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
const operationCopy = {
  verify_architecture: 'Check system architecture', verify_dgx_spark: 'Detect DGX Spark', verify_memory_capacity: 'Check memory capacity',
  verify_disk: 'Reserve disk space', verify_port: 'Check endpoint port', verify_docker: 'Check Docker', verify_nvidia_runtime: 'Check NVIDIA runtime',
  verify_artifact_access: 'Check model access', pull_image: 'Prepare vLLM runtime', download_artifact: 'Download model files',
  write_generated_config: 'Write runtime configuration', create_container: 'Create model service', stop_container: 'Stop active model',
  verify_memory: 'Reserve runtime memory', start_container: 'Start model service', wait_http: 'Verify health endpoint',
  verify_openai_inference: 'Run inference test', remove_container: 'Remove model service', remove_artifact_if_unshared: 'Remove model files'
};
const stateCopy = {
  queued: 'Queued', preflighting: 'Checking system', downloading_runtime: 'Preparing runtime', downloading_models: 'Downloading model',
  configuring: 'Configuring', checking_memory: 'Reserving memory', starting: 'Starting model', stopping: 'Stopping model',
  verifying_health: 'Checking health', verifying_inference: 'Testing inference', removing: 'Removing model', ready: 'Ready',
  stopped: 'Stopped', removed: 'Removed', failed: 'Failed', cancelled: 'Cancelled', cancelling: 'Cancelling', interrupted: 'Interrupted',
  rolling_back: 'Restoring previous model'
};

async function copyText(value) {
  // navigator.clipboard only exists in secure contexts; plain-HTTP LAN access
  // (the documented deployment) needs the selection fallback.
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(value);
  const holder = document.createElement('textarea');
  holder.value = value;
  holder.setAttribute('readonly', '');
  holder.style.position = 'fixed';
  holder.style.opacity = '0';
  document.body.appendChild(holder);
  holder.select();
  try {
    if (!document.execCommand('copy')) throw new Error('Copy is unavailable in this browser context.');
  } finally { holder.remove(); }
}

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

$('#refresh').addEventListener('click', refresh);

async function refresh() {
  try {
    const [system, recipes, models, jobs] = await Promise.all([
      api('/api/v1/system'), api('/api/v1/recipes'), api('/api/v1/models'), api('/api/v1/jobs')
    ]);
    Object.assign(state, { system, recipes, models, jobs });
    renderSystem(system);
    renderRecipes();
    renderJobs();
    const selected = state.jobs.find(job => job.id === state.selectedJobID);
    if (selected) renderDeployment(selected);
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
    const selected = state.jobs.find(job => job.id === state.selectedJobID);
    if (selected) renderDeployment(selected);
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
      if (state.selectedJobID === job.id) renderDeployment(job);
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
  $('#system-summary').textContent = `${system.product_name || 'Unknown hardware'} / ${system.architecture}`;
  $('#memory').textContent = formatBytes(system.memory_available_bytes);
  $('#storage').textContent = formatBytes(system.storage_available_bytes);
  const scope = system.hardware_scope || { mode: 'local-manager', detected_spark_count: system.dgx_spark ? 1 : 0, managed_nodes: [] };
  const count = scope.detected_spark_count || 0;
  $('#hardware-title').textContent = count === 1 ? '1 Spark detected' : count > 1 ? `${count} Sparks detected` : 'No Spark detected';
  $('#hardware-copy').textContent = count ? 'Models below are matched to detected capacity.' : 'Run the manager on a DGX Spark to unlock deployments.';
  $('#discovery-mode').textContent = scope.mode === 'local-manager' ? 'Local discovery' : 'Cluster discovery';
  const nodes = scope.managed_nodes || [];
  $('#detected-nodes').innerHTML = nodes.map(node => `<article class="detected-node ${node.ready ? 'ready' : ''}">
    <span class="node-glyph" aria-hidden="true"><i></i></span>
    <div><strong>${escapeHTML(node.hostname || 'Local manager')}</strong><span>${escapeHTML(node.product_name || 'Hardware not identified')}</span></div>
    <b>${node.ready ? 'Ready' : node.dgx_spark ? 'Needs setup' : 'Not a Spark'}</b>
  </article>`).join('') || '<p class="empty-copy">No managed nodes reported.</p>';
  const blockers = system.blocking_conditions || [];
  $('#blockers').classList.toggle('hidden', !blockers.length);
  $('#blocker-list').innerHTML = blockers.map(value => `<li>${escapeHTML(value)}</li>`).join('');
}

function catalogueState() {
  const models = state.models.map(({ recipe_id, status, active }) => [recipe_id, status, active]);
  const busy = state.jobs.filter(job => !terminal(job.state)).map(job => job.recipe_id).sort();
  const capacity = state.system?.hardware_scope?.detected_spark_count || 0;
  return JSON.stringify([capacity, state.recipes.map(item => [item.id, item.version]), models, busy, [...state.pendingModels].sort()]);
}

function setPending(id, pending) {
  if (pending) state.pendingModels.add(id); else state.pendingModels.delete(id);
  state.catalogueFingerprint = '';
  renderRecipes();
}

function renderRecipes() {
  const fingerprint = catalogueState();
  if (fingerprint === state.catalogueFingerprint) return;
  state.catalogueFingerprint = fingerprint;
  const recipesElement = $('#recipes');
  const detected = state.system?.hardware_scope?.detected_spark_count || 0;
  $('#recipe-count').textContent = String(state.recipes.length);
  $('#catalog-note').textContent = detected ? 'Matched to detected hardware' : 'Single-Spark recipes';

  const installed = new Map(state.models.map(model => [model.recipe_id, model]));
  const order = ['qwen36-35b-a3b-nvfp4-1s', 'qwen36-27b-nvfp4-1s', 'laguna-s-2-1-nvfp4-dflash-1s'];
  const recipes = [...state.recipes].sort((a, b) => order.indexOf(a.id) - order.indexOf(b.id));
  recipesElement.innerHTML = recipes.map(item => {
    const model = installed.get(item.id);
    const busy = state.pendingModels.has(item.id) || state.jobs.some(job => job.recipe_id === item.id && !terminal(job.state));
    const copy = productCopy[item.id] || { name: item.display_name, mark: 'AI', verdict: 'Ready for your Spark.', pace: 'Not measured', use: 'Local model' };
    const status = busy ? 'Working' : model ? model.status : 'Not installed';
    const activeOther = state.models.find(candidate => candidate.active && candidate.recipe_id !== item.id);
    const hardwareFits = detected >= item.topology.spark_count;
    let primaryAction;
    let utilityActions = '';
    if (!model) primaryAction = `<button data-action="install" data-id="${item.id}" ${busy || !hardwareFits ? 'disabled' : ''}>${busy ? 'Working' : hardwareFits ? 'Install' : 'Needs Spark'}</button>`;
    else if (model.active && model.status === 'ready') {
      primaryAction = `<button class="secondary" data-action="stop" data-id="${item.id}" ${busy ? 'disabled' : ''}>Stop</button>`;
      utilityActions = `<button class="secondary" data-action="smoke-test" data-id="${item.id}">Test</button><button class="secondary" data-action="copy-endpoint" data-id="${item.id}">Copy endpoint</button><button class="secondary" data-action="copy-model" data-id="${item.id}">Copy model ID</button>`;
    } else if (model.status === 'recovering') primaryAction = '<button disabled>Recovering</button>';
    else {
      primaryAction = `<button data-action="start" data-id="${item.id}" ${busy ? 'disabled' : ''}>${activeOther ? 'Switch' : 'Start'}</button>`;
      utilityActions = `<button class="danger" data-action="remove" data-id="${item.id}">Remove</button>`;
    }
    const artifacts = item.artifacts.map(artifact => `<li><span>${escapeHTML(artifact.role)}</span><code>${escapeHTML(artifact.repository)}@${escapeHTML(artifact.revision.slice(0, 12))}</code></li>`).join('');
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
    const item = state.recipes.find(recipe => recipe.id === job.recipe_id);
    const name = productCopy[job.recipe_id]?.name || item?.display_name || job.recipe_id;
    const activeStep = [...job.steps].reverse().find(step => step.state === 'running') || [...job.steps].reverse().find(step => step.state === 'completed');
    const detail = activeStep ? operationCopy[activeStep.operation] || activeStep.operation : 'Waiting for manager';
    return `<button class="job" type="button" data-job-id="${escapeHTML(job.id)}">
      <span class="job-name"><strong>${escapeHTML(job.kind)} ${escapeHTML(name)}</strong><small>${escapeHTML(detail)}</small></span>
      <span class="job-state ${job.state === 'failed' ? 'failed' : ''}">${escapeHTML(stateCopy[job.state] || job.state)}</span>
      <span class="job-open">View deployment</span>
    </button>`;
  }).join('');
  document.querySelectorAll('[data-job-id]').forEach(button => { button.onclick = () => openDeployment(button.dataset.jobId); });
}

function phasePlan(job) {
  const checks = ['verify_architecture', 'verify_dgx_spark', 'verify_memory_capacity', 'verify_disk', 'verify_port', 'verify_docker', 'verify_nvidia_runtime', 'verify_artifact_access'];
  if (job.kind === 'install') return [
    { title: 'Check system', note: 'Hardware, memory, disk and access', states: ['queued', 'preflighting'], operations: checks },
    { title: 'Prepare runtime', note: 'Pinned vLLM image', states: ['downloading_runtime'], operations: ['pull_image'] },
    { title: 'Download model', note: 'Resumable model files', states: ['downloading_models'], operations: ['download_artifact'] },
    { title: 'Configure service', note: 'Owned configuration and container', states: ['configuring'], operations: ['write_generated_config', 'create_container'] },
    { title: 'Start model', note: 'Safe memory reservation', states: ['checking_memory', 'starting', 'stopping'], operations: ['stop_container', 'verify_memory', 'start_container'] },
    { title: 'Verify endpoint', note: 'Health and real inference', states: ['verifying_health', 'verifying_inference'], operations: ['wait_http', 'verify_openai_inference'] }
  ];
  if (job.kind === 'start') return [
    { title: 'Reserve hardware', note: 'Stop active model and check memory', states: ['queued', 'stopping', 'checking_memory'], operations: ['stop_container', 'verify_memory'] },
    { title: 'Start model', note: 'Launch the pinned runtime', states: ['starting'], operations: ['start_container'] },
    { title: 'Verify endpoint', note: 'Health and real inference', states: ['verifying_health', 'verifying_inference'], operations: ['wait_http', 'verify_openai_inference'] }
  ];
  if (job.kind === 'remove') return [
    { title: 'Stop model', note: 'End the running service', states: ['queued', 'stopping'], operations: ['stop_container'] },
    { title: 'Remove runtime', note: 'Delete owned container state', states: ['removing'], operations: ['remove_container'] },
    { title: 'Reclaim storage', note: 'Delete only unshared model files', states: ['removing'], operations: ['remove_artifact_if_unshared'] }
  ];
  if (job.kind === 'smoke-test') return [
    { title: 'Check endpoint', note: 'Wait for a healthy response', states: ['queued', 'verifying_health'], operations: ['wait_http'] },
    { title: 'Run inference', note: 'Require a non-empty model response', states: ['verifying_inference'], operations: ['verify_openai_inference'] }
  ];
  return [{ title: 'Stop model', note: 'End the running service', states: ['queued', 'stopping'], operations: ['stop_container'] }];
}

function deploymentTitle(job) {
  const item = state.recipes.find(recipe => recipe.id === job.recipe_id);
  const name = productCopy[job.recipe_id]?.name || item?.display_name || job.recipe_id;
  const verb = { install: 'Deploy', start: 'Start', stop: 'Stop', remove: 'Remove', 'smoke-test': 'Test' }[job.kind] || 'Manage';
  return `${verb} ${name}`;
}

function activePhaseIndex(job, phases) {
  if (terminal(job.state) && job.state !== 'failed' && job.state !== 'cancelled') return phases.length;
  const failedStep = [...job.steps].reverse().find(step => step.state === 'failed');
  if (failedStep) {
    const failedIndex = phases.findIndex(phase => phase.operations.includes(failedStep.operation));
    if (failedIndex >= 0) return failedIndex;
  }
  const stateIndex = phases.findIndex(phase => phase.states.includes(job.state));
  return stateIndex >= 0 ? stateIndex : 0;
}

function renderDeployment(job) {
  const phases = phasePlan(job);
  const activeIndex = activePhaseIndex(job, phases);
  $('#deployment-title').textContent = deploymentTitle(job);
  $('#deployment-status').textContent = stateCopy[job.state] || job.state;
  $('#deployment-status').className = `deployment-status ${job.state}`;
  $('#deployment-id').textContent = job.id;
  $('#deployment-updated').textContent = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(job.updated_at));
  $('#deployment-steps').innerHTML = phases.map((phase, index) => {
    let status = index < activeIndex ? 'complete' : index === activeIndex ? 'active' : 'pending';
    if (job.state === 'failed' && index === activeIndex) status = 'failed';
    if (job.state === 'cancelled' && index === activeIndex) status = 'cancelled';
    if (activeIndex === phases.length) status = 'complete';
    const label = status === 'complete' ? 'Complete' : status === 'active' ? 'In progress' : status === 'failed' ? 'Failed' : status === 'cancelled' ? 'Cancelled' : 'Waiting';
    return `<li class="${status}"><i aria-hidden="true"></i><div><strong>${escapeHTML(phase.title)}</strong><span>${escapeHTML(phase.note)}</span></div><b>${label}</b></li>`;
  }).join('');
  const current = [...job.steps].reverse().find(step => step.state === 'running') || [...job.steps].reverse().find(step => step.state === 'failed');
  $('#deployment-current').textContent = current ? operationCopy[current.operation] || current.operation : terminal(job.state) ? stateCopy[job.state] || job.state : 'Waiting for manager';
  $('#deployment-error').classList.toggle('hidden', !job.error);
  $('#deployment-error').textContent = job.error || '';
  $('#deployment-receipts').innerHTML = job.steps.length ? job.steps.map(step => {
    const receipt = step.receipt && Object.keys(step.receipt).length ? JSON.stringify(step.receipt, null, 2) : 'No receipt yet';
    return `<li><div><strong>${escapeHTML(operationCopy[step.operation] || step.operation)}</strong><span>${escapeHTML(step.state)}</span></div><pre>${escapeHTML(receipt)}</pre></li>`;
  }).join('') : '<li class="receipt-empty">The first persisted step will appear here.</li>';
  $('#cancel-deployment').classList.toggle('hidden', terminal(job.state));
}

function openDeployment(jobID) {
  const job = state.jobs.find(item => item.id === jobID);
  if (!job) return;
  state.selectedJobID = jobID;
  renderDeployment(job);
  $('#deployment-dialog').showModal();
}

function acceptJob(result) {
  if (!result?.job) return;
  const index = state.jobs.findIndex(job => job.id === result.job.id);
  if (index === -1) state.jobs.unshift(result.job); else state.jobs[index] = result.job;
  state.jobFingerprint = '';
  renderJobs();
  syncJobStreams();
  openDeployment(result.job.id);
}

async function action(name, id) {
  try {
    if (name === 'install') return await confirmInstall(id);
    const item = state.recipes.find(recipe => recipe.id === id);
    if (name === 'copy-endpoint') { await copyText(`http://${location.hostname}:${item.service.default_host_port}/v1`); return; }
    if (name === 'copy-model') { await copyText(item.service.served_model_id); return; }
    if (name === 'start') {
      const active = state.models.find(model => model.active && model.recipe_id !== id);
      if (active) {
        const activeRecipe = state.recipes.find(recipe => recipe.id === active.recipe_id);
        if (!confirm(`Switch to ${productCopy[id]?.name || item.display_name}?\n\n${productCopy[active.recipe_id]?.name || activeRecipe?.display_name || active.recipe_id} will stop. If the new model fails verification, RunOnSpark will try to restore it.`)) return;
      }
    }
    let options = { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: '{}' };
    if (name === 'remove') {
      if (!confirm('Remove this model runtime and configuration?')) return;
      const removeArtifacts = confirm(`Also remove ${formatBytes(item.artifact_bytes)} of model data? Cancel keeps the download.`);
      options = { method: 'DELETE', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ remove_artifacts: removeArtifacts, expected_reclaim_bytes: removeArtifacts ? item.artifact_bytes : 0 }) };
    }
    if (state.pendingModels.has(id)) return;
    setPending(id, true);
    const result = name === 'remove' ? await api(`/api/v1/models/${id}`, options) : await api(`/api/v1/models/${id}/${name}`, options);
    acceptJob(result);
    await refreshModelsAndJobs();
  } catch (error) { alert(error.message); }
  finally { if (state.pendingModels.has(id)) setPending(id, false); }
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
  const active = state.models.find(model => model.active && model.recipe_id !== id);
  const switchNotice = active ? `<p class="switch-notice"><strong>${escapeHTML(productCopy[active.recipe_id]?.name || active.recipe_id)} will stop after the download.</strong> If this model fails verification, RunOnSpark will try to restore it.</p>` : '';
  $('#confirm-detail').innerHTML = `<dl class="install-facts"><div><dt>Download</dt><dd>${formatBytes(item.artifact_bytes)}</dd></div><div><dt>Space needed</dt><dd>${formatBytes(item.required_bytes)}</dd></div><div><dt>RAM kept free</dt><dd>${formatBytes(item.requirements.per_node_memory_reserve_bytes)}</dd></div><div><dt>Port</dt><dd>${item.service.default_host_port}</dd></div></dl>${switchNotice}<a href="${escapeHTML(item.artifacts[0].licence_url)}" target="_blank" rel="noreferrer">Read the ${escapeHTML(item.artifacts[0].licence)} licence ↗</a>`;
  $('#licence').checked = false;
  $('#confirm-dialog').showModal();
  $('#confirm-install').onclick = async event => {
    event.preventDefault();
    if (!$('#licence').checked) return alert('Accept the licence to continue.');
    if (state.pendingModels.has(id)) return;
    setPending(id, true);
    try {
      const result = await api(`/api/v1/models/${id}/install`, { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ confirmed: true, accept_licence: true }) });
      $('#confirm-dialog').close();
      acceptJob(result);
      await refreshModelsAndJobs();
    } catch (error) { alert(error.message); }
    finally { if (state.pendingModels.has(id)) setPending(id, false); }
  };
}

$('#deployment-dialog').addEventListener('close', () => { state.selectedJobID = ''; });
$('#close-deployment').addEventListener('click', () => $('#deployment-dialog').close());
$('#done-deployment').addEventListener('click', () => $('#deployment-dialog').close());
$('#cancel-deployment').addEventListener('click', async () => {
  const job = state.jobs.find(item => item.id === state.selectedJobID);
  if (!job || terminal(job.state)) return;
  try {
    await api(`/api/v1/jobs/${encodeURIComponent(job.id)}/cancel`, { method: 'POST', body: '{}' });
    await refreshModelsAndJobs();
  } catch (error) { alert(error.message); }
});

boot();
