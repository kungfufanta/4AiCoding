// Browser smoke test of the console against a running stack.
//   BASE=http://console.localhost:18080 ADMIN_USER=admin ADMIN_PASS=... SHOTS=./shots node smoke.cjs
// Signs in (or runs first-run setup), lands on the overview, creates a sandbox from the
// create form, runs a command on its terminal page,
// checks metrics, tries an egress change the policy refuses, edits the
// policy, opens every admin tab, verifies the audit chain, mints an API key,
// opens the overview, monitoring, node resources and users pages, deletes the sandbox and signs out. Screenshots of each step go to SHOTS.
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
  const setup = page.getByRole("button", { name: /Create admin/ });
  const login = page.getByRole("button", { name: /Sign in/ });
  await Promise.race([setup.waitFor(), login.waitFor()]);
  await page.getByLabel("Username").fill(USER);
  await page.getByLabel(/^Password/).fill(PASS);
  if (await setup.isVisible()) await page.getByLabel(/^Confirm password/).fill(PASS);
  await shot("login");
  await page.locator("form button[type=submit]").click();
  await page.getByRole("heading", { name: /^Welcome back/ }).waitFor();
  await page.getByRole("heading", { name: "Recent Sandboxes" }).waitFor();
  await shot("overview");

  // sidebar entries are buttons whose name also carries the Vietnamese hint
  const side = (name) => page.getByRole("navigation", { name: "Main" }).getByRole("button", { name: new RegExp("^" + name) });

  step("create sandbox");
  await side("Sandboxes").click();
  await page.getByRole("heading", { name: "Sandboxes", exact: true }).waitFor();
  await page.getByRole("button", { name: "New Sandbox" }).click();
  await page.getByRole("heading", { name: "Create New Sandbox" }).waitFor();
  await page.getByRole("button", { name: "Add variable" }).click();
  await page.getByLabel("Variable name").fill("HELLO");
  await page.getByLabel("Variable value").fill("world");
  await shot("create");
  await page.getByRole("button", { name: "Create sandbox" }).click();
  const open = page.getByRole("button", { name: /^Open sbx-/ });
  await open.waitFor({ timeout: 120000 });
  const id = (await open.innerText()).trim().replace(/^Open\s+/, "");
  await open.click();
  await page.getByRole("button", { name: "Terminal" }).waitFor();
  await shot("sandbox-detail");

  step("exec + metrics (terminal page)");
  await page.getByRole("button", { name: "Terminal" }).click();
  await page.getByRole("heading", { name: /^Sandbox / }).waitFor({ timeout: 120000 });
  await page.getByPlaceholder("ls -la /home/sandbox").fill("echo $HELLO; id -u");
  await page.getByRole("button", { name: "Run" }).click();
  await page.getByText("[exit 0]").waitFor({ timeout: 60000 });
  const out = await page.locator("pre.term").first().innerText();
  if (!out.includes("world")) throw new Error("exec output missing env var: " + out);
  await page.getByText("processes").waitFor({ timeout: 20000 });
  await shot("terminal");

  step("egress change refused by policy");
  await page.getByRole("link", { name: "Network" }).click();
  await page.getByRole("heading", { name: "Network mode" }).waitFor();
  await page.getByRole("heading", { name: "Live network activity" }).waitFor();
  await page.locator("textarea").first().fill("github.com");
  await page.getByRole("button", { name: "Save network rules" }).click();
  await page.getByText(/not allowed by policy/).waitFor();
  await page.getByRole("radio", { name: /Open, monitored/ }).check();
  await page.getByText(/Open mode needs the egress policy/).waitFor();
  await shot("network");

  step("admin: policy, tabs, audit, api key");
  await side("Settings").click();
  await page.getByRole("heading", { name: "Instance policy" }).waitFor();
  await page.getByLabel("Egress allowlist").fill("github.com");
  await page.getByRole("button", { name: "Save policy" }).click();
  await page.getByText("Policy saved").waitFor();
  await shot("policy");
  for (const [tab, heading] of [["Approvals", "Approvals"], ["Users", "Users"], ["API keys", "API keys"], ["Domain", "Domain"], ["Audit log", "Audit log"]]) {
    await page.locator("nav.tabs").getByRole("link", { name: tab }).click();
    await page.getByRole("heading", { name: heading, exact: true }).waitFor();
  }
  await page.getByRole("button", { name: "Verify chain" }).click();
  await page.getByText(/Hash chain intact/).waitFor();
  await page.getByText("policy.update").first().waitFor();
  await shot("audit");
  await page.locator("nav.tabs").getByRole("link", { name: "API keys" }).click();
  await page.getByLabel("Name").fill("smoke-" + Date.now());
  await page.getByRole("button", { name: "Create key" }).click();
  await page.getByText("Copy this key now").waitFor();

  step("overview, monitoring, nodes, users");
  await side("Overview").click();
  await page.getByRole("heading", { name: "Recent Sandboxes" }).waitFor();
  await side("Monitoring").click();
  await page.getByRole("heading", { name: "Per-sandbox" }).waitFor();
  await page.getByRole("heading", { name: "Fleet CPU" }).waitFor();
  await shot("monitoring");
  await side("Nodes").click();
  await page.getByRole("heading", { name: "Node resources" }).waitFor();
  // The local node samples its host every 10 s; wait for the first meter.
  await page.getByRole("meter", { name: "CPU" }).first().waitFor({ timeout: 30000 });
  await shot("nodes");
  await page.getByRole("button", { name: "Details" }).first().click();
  await page.getByRole("heading", { name: "Change capacity" }).waitFor();
  await shot("node-detail");
  await side("Users").click();
  await page.getByRole("heading", { name: "User Management" }).waitFor();
  await shot("users");

  step("delete sandbox + sign out");
  await page.goto(BASE + "/sandboxes/" + id + "/terminal");
  await page.getByRole("button", { name: "Delete sandbox" }).click();
  await page.getByRole("heading", { name: /^Welcome back/ }).waitFor({ timeout: 60000 });
  await page.goto(BASE + "/sandboxes");
  await page.getByRole("heading", { name: "Sandboxes", exact: true }).waitFor();
  await page.getByText(id).first().waitFor({ state: "detached", timeout: 60000 });
  await page.getByRole("button", { name: "Account menu" }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await page.getByRole("button", { name: /Sign in/ }).waitFor();

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
