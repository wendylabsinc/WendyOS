// A Wendy Net topology drawn in the style of the "How Wendy Net works" film:
// square devices, one line style per kind of connection, and the path that
// carries traffic in the accent color. It is code rather than an image, so it
// follows the light and dark themes and every label stays readable text.
//
// Coordinates are in a fixed viewBox. On a narrow screen the figure scrolls
// sideways instead of shrinking its labels.

type LinkKind = 'ethernet' | 'wifi' | 'aware' | 'bluetooth' | 'internet';

type NetNode = {
  id: string;
  x: number;
  y: number;
  label: string;
  // A second line under the device, such as "Shares its uplink".
  note?: string;
  // A server on the Internet rather than a Wendy Net device.
  server?: boolean;
  // A device that has disappeared.
  gone?: boolean;
  // Draw the label under the device instead of above it.
  labelBelow?: boolean;
};

type NetLink = {
  from: string;
  to: string;
  kind: LinkKind;
  // Shown next to the line, such as "Bluetooth 4096".
  label?: string;
  // active: carries the traffic. idle: up but unused. down: broken.
  state?: 'active' | 'idle' | 'down';
  // Bows the line sideways by this many units, to separate parallel links.
  bend?: number;
  // Put the label on the other side of the line.
  labelSide?: 'above' | 'below';
};

type NetDiagramProps = {
  label: string;
  nodes: NetNode[];
  links: NetLink[];
  width?: number;
  height?: number;
  caption?: string;
};

const SIZE = 40;

const dash: Record<LinkKind, string | undefined> = {
  ethernet: undefined,
  wifi: undefined,
  aware: '7 5',
  bluetooth: '1 6',
  internet: '14 6',
};

export function NetDiagram({ label, nodes, links, width = 720, height = 240, caption }: NetDiagramProps) {
  const byId = new Map(nodes.map((node) => [node.id, node]));

  return (
    <figure className="not-prose my-6">
      <div className="overflow-x-auto border bg-fd-card">
        <svg
          viewBox={`0 0 ${width} ${height}`}
          role="img"
          aria-label={label}
          className="block h-auto w-full min-w-[560px] text-fd-foreground"
          fontFamily="inherit"
        >
          <title>{label}</title>
          {links.map((link) => {
            const from = byId.get(link.from);
            const to = byId.get(link.to);
            if (!from || !to) return null;
            return <Link key={`${link.from}-${link.to}-${link.kind}`} link={link} from={from} to={to} />;
          })}
          {nodes.map((node) => (
            <Node key={node.id} node={node} />
          ))}
        </svg>
      </div>
      {caption ? <figcaption className="mt-2 text-sm text-fd-muted-foreground">{caption}</figcaption> : null}
    </figure>
  );
}

function Link({ link, from, to }: { link: NetLink; from: NetNode; to: NetNode }) {
  const state = link.state ?? 'idle';
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const length = Math.hypot(dx, dy) || 1;
  // Unit normal, pointing up for a left-to-right line.
  const nx = dy / length;
  const ny = -dx / length;
  const bend = link.bend ?? 0;
  const cx = (from.x + to.x) / 2 + nx * bend;
  const cy = (from.y + to.y) / 2 + ny * bend;
  // The midpoint of the quadratic curve, where the label goes.
  const mx = (from.x + 2 * cx + to.x) / 4;
  const my = (from.y + 2 * cy + to.y) / 4;
  const side = link.labelSide === 'below' ? -1 : 1;
  // Horizontal text beside a slanted line crosses it unless it sits far
  // enough out for its nearer end to clear the line. Estimate the label's
  // half width from its length at the 14-unit font size.
  const halfWidth = (link.label?.length ?? 0) * 3.9;
  const offset = (8 * Math.abs(dx) + halfWidth * Math.abs(dy)) / length + 8;
  const lx = mx + nx * offset * side;
  const ly = my + ny * offset * side + 5;

  const stroke =
    state === 'active'
      ? 'stroke-fd-primary'
      : state === 'down'
        ? 'stroke-fd-muted-foreground/40'
        : 'stroke-fd-muted-foreground';
  const textClass =
    state === 'active' ? 'fill-fd-primary' : state === 'down' ? 'fill-fd-muted-foreground/60' : 'fill-fd-muted-foreground';

  return (
    <g>
      <path
        d={`M ${from.x} ${from.y} Q ${cx} ${cy} ${to.x} ${to.y}`}
        fill="none"
        strokeWidth={state === 'active' ? 3 : 2}
        strokeDasharray={dash[link.kind]}
        strokeLinecap={link.kind === 'bluetooth' ? 'round' : 'butt'}
        className={stroke}
      />
      {state === 'down' ? (
        <g className="stroke-fd-muted-foreground" strokeWidth={2.5} strokeLinecap="square">
          <line x1={mx - 7} y1={my - 7} x2={mx + 7} y2={my + 7} />
          <line x1={mx - 7} y1={my + 7} x2={mx + 7} y2={my - 7} />
        </g>
      ) : null}
      {link.label ? (
        <text
          x={lx}
          y={ly}
          textAnchor="middle"
          fontSize={14}
          strokeWidth={6}
          strokeLinejoin="round"
          paintOrder="stroke"
          className={`stroke-fd-card ${textClass}`}
        >
          {link.label}
        </text>
      ) : null}
    </g>
  );
}

function Node({ node }: { node: NetNode }) {
  const half = SIZE / 2;
  const box = node.gone
    ? 'fill-fd-card stroke-fd-muted-foreground/50'
    : 'fill-fd-background stroke-fd-foreground';
  const labelY = node.labelBelow ? node.y + half + 20 : node.y - half - 10;
  const noteY = node.labelBelow ? labelY + 18 : node.y + half + 20;

  return (
    <g>
      <rect
        x={node.x - half}
        y={node.y - half}
        width={SIZE}
        height={SIZE}
        strokeWidth={2}
        strokeDasharray={node.gone ? '4 4' : undefined}
        className={box}
      />
      {node.server ? (
        <g className="stroke-fd-muted-foreground" strokeWidth={2}>
          <line x1={node.x - 11} y1={node.y - 7} x2={node.x + 11} y2={node.y - 7} />
          <line x1={node.x - 11} y1={node.y} x2={node.x + 11} y2={node.y} />
          <line x1={node.x - 11} y1={node.y + 7} x2={node.x + 11} y2={node.y + 7} />
        </g>
      ) : null}
      <text
        x={node.x}
        y={labelY}
        textAnchor="middle"
        fontSize={16}
        fontWeight={600}
        className={node.gone ? 'fill-fd-muted-foreground/60' : 'fill-fd-foreground'}
      >
        {node.label}
      </text>
      {node.note ? (
        <text x={node.x} y={noteY} textAnchor="middle" fontSize={13} className="fill-fd-muted-foreground">
          {node.note}
        </text>
      ) : null}
    </g>
  );
}
