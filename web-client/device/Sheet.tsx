import { useEffect, useId, useRef, type ReactNode } from "react";
import { X } from "lucide-react";

type OpenSheet = { element: HTMLDialogElement; returnFocus: HTMLElement[] };
const openSheets: OpenSheet[] = [];
let unlockedBodyOverflow = "";

export default function Sheet({ open, title, description, onClose, children }: {
  open: boolean;
  title: string;
  description?: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const titleID = useId(), descriptionID = useId();
  useEffect(() => {
    const element = dialog.current;
    if (!open || !element) return;
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const entry: OpenSheet = { element, returnFocus: [...(previousFocus ? [previousFocus] : []), ...openSheets.flatMap((sheet) => sheet.returnFocus)] };
    if (openSheets.length === 0) unlockedBodyOverflow = document.body.style.overflow;
    openSheets.push(entry);
    document.body.style.overflow = "hidden";
    // Native modal dialogs trap focus and make earlier sheets and the page inert.
    element.showModal();
    element.querySelector<HTMLElement>("[data-sheet-title]")?.focus();
    return () => {
      const wasTopSheet = openSheets.at(-1) === entry;
      const index = openSheets.indexOf(entry);
      if (index !== -1) openSheets.splice(index, 1);
      element.close();
      if (!openSheets.length) document.body.style.overflow = unlockedBodyOverflow;
      if (wasTopSheet) {
        const remaining = openSheets.at(-1)?.element;
        const target = entry.returnFocus.find((candidate) => candidate.isConnected && (!remaining || remaining.contains(candidate)));
        (target || remaining?.querySelector<HTMLElement>("[data-sheet-title]"))?.focus();
      }
    };
  }, [open]);
  if (!open) return null;
  return <dialog ref={dialog} className="device-sheet" aria-labelledby={titleID} aria-describedby={description ? descriptionID : undefined}
    onCancel={(event) => { event.preventDefault(); event.stopPropagation(); onClose(); }}
    onClick={(event) => {
      if (event.target !== event.currentTarget) return;
      const bounds = event.currentTarget.getBoundingClientRect();
      if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) onClose();
    }}>
    <header className="device-sheet-heading"><div><h2 id={titleID} tabIndex={-1} data-sheet-title>{title}</h2>{description && <p id={descriptionID} className="subtle">{description}</p>}</div><button type="button" className="quiet" aria-label={`Close ${title.toLowerCase()}`} onClick={onClose}><X size={18} /></button></header>
    <div className="device-sheet-body">{children}</div>
  </dialog>;
}
