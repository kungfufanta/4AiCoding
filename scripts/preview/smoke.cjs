// Browser smoke test of the console against a running stack.
//   BASE=http://console.localhost:18080 ADMIN_USER=admin ADMIN_PASS=... SHOTS=./shots node smoke.cjs
// Signs in (or runs first-run setup), lands on the overview, creates a sandbox, runs a command,
// checks metrics, tries an egress change the policy refuses, edits the
// policy, opens every admin tab, verifies the audit chain, mints an API key,
// opens the overview, monitoring and users pages, deletes the sandbox and signs out. Screenshots of each step go to SHOTS.
const { chromium } = require("playwright");
const fs = require("fs");

const BASE = process.env.BASE || "http://console.localhost";
const USER = process.env.ADMIN_USER || "admin";
const PASS = process.env.ADMIN_PASS || "correct-horse-1";
const SHOTS = process.env.SHOTS || "shots";

(async () => {
  fs.mkdirSync(SHOTS, { recursive: true });
  const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [];
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
  page.on("response", (r) => {
    // 403 on the egress save is the expected policy denial.
    if (r.status() >= 400 && !(r.status() === 403 && r.url().endsWith("/network/egress")) && !r.url().endsWith("/v1/auth/me"))
      errors.push(`${r.status()} ${r.request().method()} ${r.url()}`);
  });
  page.on("dialog", (d) => d.accept());
  let n = 0;
  const shot = (name) => page.screenshot({ path: `${SHOTS}/${String(++n).padStart(2, "0")}-${name}.png`, fullPage: true });
  const step = (s) => console.log("── " + s);

  step("sign in");
  await page.goto(BASE + "/");
  const setup = page.getByText("Create the first admin account");
  const login = page.getByRole("button", { name: "Sign in" });
  await Promise.race([setup.waitFor(), login.waitFor()]);
  await page.getByLabel("Username").fill(USER);
  await page.getByLabel("Password", { exact: true }).fill(PASS);
  if (await setup.isVisible()) await page.getByLabel("Confirm password").fill(PASS);
  await shot("login");
  await page.locator("form button[type=submit]").click();
  await page.getByRole("heading", { name: "Overview" }).waitFor();
  await page.getByText("sandboxes running").waitFor();
  await shot("overview");
  await page.locator("nav.top").getByRole("link", { name: "Sandboxes" }).click();
  await page.getByRole("heading", { name: "Sandboxes" }).waitFor();

  step("create sandbox");
  await page.getByRole("button", { name: "New sandbox" }).click();
  await page.getByLabel("Environment").fill("HELLO=world");
  await shot("new-sandbox");
  await page.getByRole("button", { name: "Create sandbox" }).click();
  await page.getByRole("heading", { name: /^Sandbox / }).waitFor({ timeout: 120000 });

  step("exec + metrics");
  await page.getByPlaceholder("ls -la /home/sandbox").fill("echo $HELLO; id -u");
  await page.getByRole("button", { name: "Run" }).click();
  await page.getByText("[exit 0]").waitFor({ timeout: 60000 });
  const out = await page.locator("pre.term").first().innerText();
  if (!out.includes("world") || !out.includes("1000")) throw new Error("exec output: " + out);
  await page.getByText("processes").waitFor({ timeout: 20000 });
  await shot("sandbox-detail");

  step("network: egress refused by policy");
  await page.getByRole("link", { name: "Network" }).click();
  await page.getByRole("heading", { name: "Network mode" }).waitFor();
  await page.getByRole("heading", { name: "Live network activity" }).waitFor();
  await page.locator("textarea").first().fill("github.com");
  await page.getByRole("button", { name: "Save network rules" }).click();
  await page.getByText(/not allowed by policy/).waitFor();
  await page.getByRole("radio", { name: /Open, monitored/ }).check();
  await page.getByText(/Open mode needs the egress policy/).waitFor();
  await shot("network");

  step("admin: policy + tabs");
  await page.getByRole("link", { name: "Admin" }).click();
  await page.getByRole("heading", { name: "Instance policy" }).waitFor();
  await page.getByLabel("Egress allowlist").fill("github.com");
  await page.getByRole("button", { name: "Save policy" }).click();
  await page.getByText("Policy saved").waitFor();
  await shot("policy");
  for (const [tab, heading] of [["Approvals", "Approvals"], ["Users", "Users"], ["API keys", "API keys"], ["Domain", "Domain"], ["Audit log", "Audit log"]]) {
    await page.locator("nav.tabs").getByRole("link", { name: tab }).click();
    await page.getByRole("heading", { name: heading, exact: true }).waitFor();
    await shot("admin-" + tab.toLowerCase().replace(/\s+/g, "-"));
  }
  await page.getByRole("button", { name: "Verify chain" }).click();
  await page.getByText(/Hash chain intact/).waitFor();
  await page.getByText("policy.update").first().waitFor();

  step("api key");
  await page.locator("nav.tabs").getByRole("link", { name: "API keys" }).click();
  await page.getByLabel("Name").fill("smoke-" + Date.now());
  await page.getByRole("button", { name: "Create key" }).click();
  await page.getByText("Copy this key now").waitFor();

  step("overview, monitoring, users");
  await page.locator("nav.top").getByRole("link", { name: "Overview" }).click();
  await page.getByRole("heading", { name: "Recent sandboxes" }).waitFor();
  await page.locator("nav.top").getByRole("link", { name: "Monitoring" }).click();
  await page.getByRole("heading", { name: "Per-sandbox stats" }).waitFor();
  await page.getByText("running of").waitFor();
  await shot("monitoring");
  await page.locator("nav.top").getByRole("link", { name: "Users" }).click();
  await page.getByRole("heading", { name: /^Accounts/ }).waitFor();
  await shot("users");

  step("delete sandbox + sign out");
  await page.locator("nav.top").getByRole("link", { name: "Sandboxes" }).click();
  await page.getByRole("button", { name: "Delete" }).first().click();
  await page.getByText("No sandboxes yet.").waitFor({ timeout: 60000 });
  await page.getByRole("button", { name: "Sign out" }).click();
  await page.getByRole("button", { name: "Sign in" }).waitFor();

  await browser.close();
  if (errors.length) {
    console.error("unexpected errors:\n" + errors.join("\n"));
    process.exit(1);
  }
  console.log("✅ CONSOLE SMOKE PASSED");
})().catch((e) => {
  console.error("❌ FAIL:", e.message);
  process.exit(1);
});
