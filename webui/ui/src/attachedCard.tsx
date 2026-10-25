import {
  ATTACHED_ROLE, guardLine, healthRead, kindLabel, licenceHref, nodeLabel, territoryLine, topologyLine,
  type AttachedProblem, type AttachedService,
} from './attached'

// One attached service, as one card. It carries no button: basement does not
// install, start, stop or update the service, so there is nothing on this
// card for it to do. The pieces are the console's own: the Spark card's frame,
// the facts list of an opened row, the status dot, and the node chip, which is
// lit while the service answers its health check.
export function AttachedCard({ service, nowMs }: { service: AttachedService; nowMs: number }) {
  const { declaration } = service
  const health = healthRead(service.health, nowMs)
  const territories = territoryLine(declaration)
  const guard = guardLine(service)
  const href = licenceHref(declaration)
  const live = service.health.status === 'healthy'
  return (
    <section className="node-card attached-card" aria-label={`${declaration.display_name}, attached service`}>
      <header>
        <div className="who">
          <strong>{declaration.display_name}</strong>
          <span className="role">{ATTACHED_ROLE}</span>
        </div>
        <span className="attached-health">
          <i className={`sdot ${health.dot}`} aria-hidden="true" />
          {health.text}
        </span>
      </header>
      <div className="attached-body">
        <dl className="facts">
          <dt>Runs on</dt>
          <dd>
            {topologyLine(declaration)}
            <span className="attached-nodes">
              {service.nodes.map(node => (
                <span key={node.name} className={`node-chip ${live ? 'live' : ''}`}>
                  <i aria-hidden="true" />
                  {nodeLabel(node)}
                </span>
              ))}
            </span>
          </dd>
          {service.health.status === 'unhealthy' && service.health.last_error && (
            <>
              <dt>Last check</dt>
              <dd>{service.health.last_error}</dd>
            </>
          )}
          <dt>Endpoint</dt>
          <dd>{declaration.endpoint ? <code>{declaration.endpoint}</code> : 'n/a'}</dd>
          <dt>Managed by</dt>
          <dd>{declaration.managed_by}</dd>
          <dt>Model by</dt>
          <dd>{declaration.model_by || 'n/a'}</dd>
          <dt>Kind</dt>
          <dd>{kindLabel(declaration.kind)}</dd>
          <dt>Served as</dt>
          <dd>{declaration.served_model_name ? <code>{declaration.served_model_name}</code> : 'n/a'}</dd>
          <dt>Licence</dt>
          <dd>
            {href
              ? <a href={href} target="_blank" rel="noopener noreferrer">{declaration.licence}</a>
              : declaration.licence || 'n/a'}
          </dd>
        </dl>
        {territories && <p className="table-note warn">{territories}</p>}
        {declaration.notes && <p className="attached-notes">{declaration.notes}</p>}
        <p className={`table-note ${guard.warn ? 'warn' : ''}`}>{guard.text}</p>
      </div>
    </section>
  )
}

// Declaration files the manager could not use. Each one keeps no Spark free,
// which is the part the owner most needs to know.
export function AttachedProblems({ problems }: { problems: AttachedProblem[] }) {
  return (
    <div className="alert" role="alert">
      <strong>An attached service declaration was not used</strong>
      <ul>
        {problems.map(problem => (
          <li key={problem.file}><code>{problem.file}</code>: {problem.error}</li>
        ))}
      </ul>
      <p className="attached-fix">
        Basement keeps no Spark free for a file it cannot read. Fix the file in attached-services.d and
        it is read again within a minute.
      </p>
    </div>
  )
}
