// Shared formatters for the dashboard.

export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

export function shortID(id: string): string {
  return id.length > 8 ? id.slice(0, 8) + "…" : id;
}
