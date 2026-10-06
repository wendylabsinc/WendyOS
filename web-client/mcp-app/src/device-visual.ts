// Display hints only. Device permissions and capabilities always come from the
// gateway/agent; recognizing a name here never enables a control.
export function displayModel(id?: string, name = ""): string {
  if (id && id !== "generic") return id;
  const label = name.toLowerCase().replace(/[-_]/g, " ");
  if (/\bunitree\s+g1\b/.test(label)) return "g1";
  if (/\bunitree\s+go2\b/.test(label)) return "go2";
  if (
    /\bdgx\s*spark\b/.test(label) ||
    /^spark\s+(?:[a-f0-9]{4}|[0-9]+)\b/.test(label)
  )
    return "dgx";
  return "generic";
}
