import {
  ATTACHED_ROLE, guardLine, healthRead, kindLabel, licenceHref, territoryLine, topologyLine,
  type AttachedProblem, type AttachedService,
} from './attached'
import { Mark } from './mark'

// One attached service, as one more serving card: the same frame, mark,
// status chip and box chips as a model basement serves, with the green edge
// only while the service answers its health check. It carries no button:
// basement does not install, start, stop or update the service, so there is
// nothing on this card for it to do. The card opens on the declaration the
// way a serving card opens on its model, and it opens by itself when a Spark
// it names cannot be kept free, because that is the part the owner must fix.
export function AttachedCard({ service, nowMs }: { service: AttachedService; nowMs: number }) {
  const { declaration } = service
  const health = healthRead(service.health, nowMs)
  const territories = territoryLine(declaration)
  const guard = guardLine(service)
  const href = licenceHref(declaration)
  const live = health.tone === 'live'
  return (
    <details className="mband-group attached" open={guard.warn} aria-label={`${declaration.display_name}, attached service`}>
      <summary>
        <span className={`mband attached-band ${live ? 'live' : ''}`}>
          <span className="m-id">
            <Mark name={declaration.display_name} size={36} />
            <span className="mb-main">
              <span className="nm">
                {declaration.display_name}
                <span className={`state-chip ${health.tone}`}><i aria-hidden="true" />{health.word}</span>
              </span>
              <span className="meta">
                {health.parts.length > 0 && (
                  <span className="mb-live">
                    {health.parts.map((part, index) => (
                      <span key={part} className={index > 0 ? 'mb-part' : undefined}>{part}</span>
                    ))}
                  </span>
                )}
                <span className="mb-place">{topologyLine(declaration)}</span>
              </span>
              {/* Every Spark the declaration names, with the role it gives
                  it. Lit while the service answers; amber when this console
                  cannot match the name, so cannot keep its models off it. */}
              <span className="members" role="group" aria-label="Sparks it runs on">
                {service.nodes.map(node => (
                  <span key={node.name} className="member-chip attached-node">
                    <i className={`hdot ${!node.protected ? 'warn' : live ? 'ok' : 'unknown'}`} aria-hidden="true" />
                    <span className="mc-name">{node.name}</span>
                    {node.role && <small>{node.role}</small>}
                  </span>
                ))}
              </span>
            </span>
          </span>
          <span className="attached-role">{ATTACHED_ROLE}</span>
        </span>
      </summary>
      <div className="mdetail attached-detail">
        <dl className="facts">
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
    </details>
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
