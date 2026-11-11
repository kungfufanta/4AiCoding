// Tiny class joiner — keeps component files readable without pulling
// in a class-merge dependency (PORTAL_PLAN_V2 §2 bundle budget).
export function cn(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}
