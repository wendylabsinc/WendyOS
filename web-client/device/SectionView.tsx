import type { WendyClient } from "@/lib/client";
import type { Operation } from "./operations";
import OperationCard from "./OperationCard";
export default function SectionView({ client, operations, refresh, title }: { client: WendyClient; operations: Operation[]; refresh: number; title?: string }) {
  const reads = operations.filter((op) => op.automatic), actions = operations.filter((op) => !op.automatic);
  return <>{reads.length > 0 && <div className="device-cards">{reads.map((op) => <OperationCard key={op.service + op.method + op.title} client={client} operation={op} refresh={refresh} />)}</div>}{actions.length > 0 && <section className="panel settings-panel">{title && <div className="settings-heading"><h2>{title}</h2></div>}{actions.map((op) => <OperationCard key={op.service + op.method + op.title} client={client} operation={op} refresh={refresh} compact />)}</section>}</>;
}
