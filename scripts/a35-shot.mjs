// a35-shot.mjs — screenshots of the finished agent run on the
// production portal (the "ảnh" of the A3.5 acceptance). Reads the
// ids.json + curl cookie jar the acceptance script produced and
// writes two PNGs (light + dark) of the chat with the run card.
// Usage: node a35-shot.mjs <ids.json> <cookies.txt> <out-prefix>
import { chromium } from "@playwright/test";
import { readFileSync } from "node:fs";

const [idsPath, jarPath, outPrefix] = process.argv.slice(2);
if (!idsPath || !jarPath || !outPrefix) {
  console.error("usage: node a35-shot.mjs <ids.json> <cookies.txt> <out-prefix>");
  process.exit(1);
}
const ids = JSON.parse(readFileSync(idsPath, "utf8"));

// Netscape jar → Playwright storageState cookies.
const cookies = readFileSync(jarPath, "utf8")
  .split("\n")
  .filter((l) => l.trim() && l.includes("\t") && !l.startsWith("# ") && !l.startsWith("#HttpOnly_ #"))
  .map((l) => l.split("\t"))
  .filter((f) => f[5] === "portal_session" || f[5] === "portal_csrf")
  .map((f) => ({
    name: f[5],
    value: f[6],
    domain: f[0].replace(/^#HttpOnly_/, "").replace(/^\./, ""),
    path: f[2] || "/",
    secure: f[3] === "TRUE",
    httpOnly: f[0].startsWith("#HttpOnly_"),
    sameSite: "Lax",
  }));
if (!cookies.some((c) => c.name === "portal_session")) {
  console.error("no portal_session cookie in the jar");
  process.exit(1);
}

const url = `${ids.base}/orgs/${ids.org}/chat/${ids.chat}`;
const browser = await chromium.launch();
const context = await browser.newContext({
  storageState: { cookies, origins: [] },
  viewport: { width: 1440, height: 900 },
});
const page = await context.newPage();
await page.goto(url, { waitUntil: "domcontentloaded" });
const result = page.getByTestId("agent-result");
await result.waitFor({ state: "visible", timeout: 60_000 });
await page.waitForTimeout(1000);
await page.screenshot({ path: `${outPrefix}-chat.png`, fullPage: false });

// Dark mode shot of the same run.
await page.getByRole("button", { name: "Toggle dark mode" }).click();
await page.waitForTimeout(500);
await page.screenshot({ path: `${outPrefix}-chat-dark.png`, fullPage: false });

console.log(`screenshots: ${outPrefix}-chat.png, ${outPrefix}-chat-dark.png  (${url})`);
await browser.close();
