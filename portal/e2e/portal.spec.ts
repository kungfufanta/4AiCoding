// One screenshot per screen lands in test-results/preview and the
// preview-portal workflow uploads it as the run artifact.

import { expect, test, type Page } from "@playwright/test";

const SHOTS = "test-results/preview";
// Unique per run so reruns against a persisted compose volume never
// hit slug/email conflicts (409).
const runStamp = String(Date.now());

async function shot(page: Page, name: string) {
  await page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: true });
}

// "/" redirects to the first org's projects; return the slug for the
// org-scoped pages.
async function gotoOrgBase(page: Page): Promise<string> {
  await page.goto("/");
  await expect(page).toHaveURL(/\/orgs\/([^/]+)\/projects/);
  return page.url().match(/\/orgs\/([^/]+)\/projects/)![1];
}

test.describe.serial("portal preview", () => {
  test("projects page and create-project dialog", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
    await shot(page, "01-projects");

    await page.getByRole("button", { name: "New project" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("New project")).toBeVisible();
    await dialog.getByPlaceholder("Agent evaluation").fill(`Preview e2e ${runStamp}`);
    await page.getByRole("button", { name: "Create project", exact: true }).click();
    await expect(page.getByText(`Preview e2e ${runStamp}`).first()).toBeVisible();
    await shot(page, "02-project-created");
  });

  test("members page, invite dialog and once-shown link", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await page.goto(`/orgs/${slug}/members`);
    await expect(page.getByRole("heading", { name: "Members" })).toBeVisible();
    await expect(page.getByRole("cell", { name: "admin@portal.local" })).toBeVisible();

    await page.getByRole("button", { name: "Invite", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Invite a member")).toBeVisible();
    await dialog.getByPlaceholder("teammate@company.com").fill(`preview+${runStamp}@example.com`);
    await page.getByRole("button", { name: "Send invite", exact: true }).click();
    await expect(page.getByText("shown only once")).toBeVisible();
    await page.waitForTimeout(250); // let the dialog fade-in settle
    await shot(page, "03-invite-url");
    await page.getByRole("button", { name: "Done", exact: true }).click();

    await expect(page.getByRole("button", { name: "Invite", exact: true })).toBeVisible();
    await shot(page, "04-members");
  });

  test("audit trail shows recorded events", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await page.goto(`/orgs/${slug}/audit`);
    await expect(page.getByRole("heading", { name: "Audit trail" })).toBeVisible();
    await expect(page.getByText("org.create").first()).toBeVisible();
    await shot(page, "05-audit");
  });

  test("dark mode toggle", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await page.getByRole("button", { name: "Toggle dark mode" }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    // Let the 150ms color transition settle so the screenshot shows
    // the final palette, not a mid-fade frame.
    await page.waitForTimeout(300);
    await shot(page, "06-dark-mode");
    await page.getByRole("button", { name: "Toggle dark mode" }).click();
    await expect(page.locator("html")).not.toHaveClass(/dark/);
  });

  test("mobile layout does not overflow", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`/orgs/${slug}/members`);
    await expect(page.getByRole("heading", { name: "Members" })).toBeVisible();
    await shot(page, "07-mobile");

    await page.getByRole("button", { name: "Open navigation" }).click();
    await expect(page.getByRole("dialog", { includeHidden: false }).first()).toBeVisible();
    await shot(page, "08-mobile-drawer");
    await page.setViewportSize({ width: 1280, height: 800 });
  });

  test("api keys — create, secret shown once, revoke", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await page.goto(`/orgs/${slug}/api-keys`);
    await expect(page.getByRole("heading", { name: "API keys" })).toBeVisible();
    await shot(page, "10-api-keys");

    await page.getByRole("button", { name: "Create key", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Create API key")).toBeVisible();
    await dialog.getByPlaceholder("CI pipeline").fill(`Preview key ${runStamp}`);
    await dialog.getByRole("checkbox", { name: "sandbox:exec" }).check();
    await page.getByRole("button", { name: "Create key", exact: true }).click();

    // The secret is shown exactly once, in a copyable field.
    await expect(page.getByText("Save your API key now")).toBeVisible();
    const secret = (await page.getByRole("dialog").locator("code").textContent()) ?? "";
    expect(secret).toContain("sbx_live_");
    await page.waitForTimeout(250); // let the dialog fade-in settle
    await shot(page, "11-api-key-secret");
    await page.getByRole("button", { name: "I saved it", exact: true }).click();

    await expect(page.getByText(`Preview key ${runStamp}`)).toBeVisible();

    // After a reload the secret is gone for good.
    await page.reload();
    await expect(page.getByRole("heading", { name: "API keys" })).toBeVisible();
    expect(await page.content()).not.toContain(secret);

    // Revoke goes through a confirm dialog.
    const row = page.getByRole("row", { name: new RegExp(`Preview key ${runStamp}`) });
    await row.getByRole("button", { name: "Revoke" }).click();
    const confirmDialog = page.getByRole("dialog");
    await expect(confirmDialog.getByText("lose access within 60 seconds")).toBeVisible();
    await confirmDialog.getByRole("button", { name: "Revoke key", exact: true }).click();
    await expect(page.getByText("revoked").first()).toBeVisible();
    await shot(page, "12-api-keys-revoked");
  });

  test("sandboxes — create, exec output, delete with confirm (P4)", async ({ page }) => {
    const slug = await gotoOrgBase(page);
    await page.goto(`/orgs/${slug}/sandboxes`);
    await expect(page.getByRole("heading", { name: "Sandboxes" })).toBeVisible();
    await shot(page, "13-sandboxes");

    // Create a sandbox on the profile's allowlisted e2e image.
    await page.getByRole("button", { name: "New sandbox" }).first().click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("New sandbox")).toBeVisible();
    await dialog.getByPlaceholder("ubuntu:24.04").fill("sandboxd-e2e:ci");
    await dialog.getByRole("button", { name: "Create sandbox", exact: true }).click();

    // The dialog navigates to the detail page; the ULID id heads it and
    // the status pill settles on running.
    await expect(page.getByRole("heading", { name: /0[0-9A-Z]{10,}/ })).toBeVisible({
      timeout: 30_000,
    });
    await expect(page.getByText("running").first()).toBeVisible({ timeout: 30_000 });
    await page.waitForTimeout(300);
    await shot(page, "14-sandbox-detail");

    // Exec tab: run a command, expect its output plus the exit badge.
    await page.getByRole("tab", { name: "Exec" }).click();
    await page.getByLabel("Command").fill("echo preview-e2e-ok");
    await page.getByRole("button", { name: "Run", exact: true }).click();
    await expect(page.getByText("preview-e2e-ok")).toBeVisible({ timeout: 60_000 });
    await expect(page.getByText("exit 0")).toBeVisible();
    await shot(page, "15-sandbox-exec");

    // Delete goes through a confirm dialog that spells out the blast
    // radius, then lands back on the list.
    await page.getByRole("button", { name: "Delete", exact: true }).click();
    const confirmDialog = page.getByRole("dialog");
    await expect(confirmDialog.getByText("Delete this sandbox?")).toBeVisible();
    await confirmDialog.getByRole("button", { name: "Delete sandbox", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/orgs/${slug}/sandboxes$`), { timeout: 30_000 });
    await shot(page, "16-sandbox-list-after-delete");
  });

  test("invite page handles an unknown token", async ({ page }) => {
    await page.goto("/invites/00000000-0000-0000-0000-000000000000?token=not-a-real-token");
    await expect(page.getByText("Your invitation")).toBeVisible();
    await page.getByRole("button", { name: "Accept invite", exact: true }).click();
    await expect(
      page.getByText("This invite is not valid anymore — it expired or was already used."),
    ).toBeVisible();
    await shot(page, "09-invite-invalid");
  });

  // A2 phase 3 follow-up: the bug fix for "chat portal chưa chat được".
  // The chat status route is org-scoped so a DB-configured platform
  // connection makes chat usable without PORTAL_DEFAULT_LLM_API_KEY.
  // The Playwright stack has no PORTAL_DEFAULT_LLM_API_KEY set, so
  // before this fix the chat textarea was always disabled.
  test("admin LLM connection wires up chat (A2 phase 3)", async ({ page }) => {
    const slug = await gotoOrgBase(page);

    // 1. Chat is initially disabled (no env LLM, no DB connection yet).
    await page.goto(`/orgs/${slug}/chat`);
    await expect(page.getByText("Chat is not configured yet")).toBeVisible();
    await shot(page, "20-chat-disabled");

    // 2. Admin adds a platform connection pointing at the public WebHub
    //    demo endpoint. The demo key is rate-limited but fine for one
    //    probe + one chat reply.
    await page.goto(`/orgs/${slug}/admin/llm`);
    await expect(page.getByRole("heading", { name: "LLM connections" })).toBeVisible();
    await shot(page, "21-admin-llm-empty");

    await page.getByRole("button", { name: "New connection" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("New LLM connection")).toBeVisible();
    await dialog.getByLabel("Name").fill("webhub-demo");
    // Flavor = WebHub so the tenant headers (X-WebHub-User etc.) ride
    // along — this is what the production default would use.
    await dialog.getByLabel("Flavor").click();
    await page.getByRole("option", { name: "WebHub" }).click();
    await dialog.getByPlaceholder("https://h11hp9dd1y90-d.space-z.ai/v1").fill(
      "https://h11hp9dd1y90-d.space-z.ai/v1",
    );
    await dialog.getByPlaceholder("glm-4.6").fill("glm-4.6");
    await dialog.getByPlaceholder("wh_… or sk_…").fill("wh_demo_key_local");
    await dialog.getByRole("checkbox", { name: "Set as default" }).check();
    await page.getByRole("button", { name: "Save connection" }).click();
    await expect(page.getByText("webhub-demo")).toBeVisible({ timeout: 10_000 });
    await shot(page, "22-admin-llm-created");

    // 3. Test the connection — a tiny probe. The toast announces
    //    success with the model + latency.
    await page.getByRole("row", { name: /webhub-demo/ }).getByRole("button", { name: "Test" }).click();
    await expect(page.getByText(/Test OK/).first()).toBeVisible({ timeout: 30_000 });
    await shot(page, "23-admin-llm-test-ok");

    // 4. Chat is now wired. The textarea is enabled and the empty
    //    state offers the suggestion buttons.
    await page.goto(`/orgs/${slug}/chat`);
    await expect(page.getByPlaceholder(/Nhắn tin/)).toBeEnabled();
    await expect(page.getByText("Hôm nay bạn muốn làm gì?")).toBeVisible();
    await shot(page, "24-chat-enabled");

    // 5. Send a message and wait for the streamed reply to land. The
    //    WebHub demo endpoint streams "Pong" (or similar) within a
    //    few seconds; the assistant bubble appears once the first
    //    delta arrives.
    await page.getByPlaceholder(/Nhắn tin/).fill("Say pong");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText(/pong/i).first()).toBeVisible({ timeout: 30_000 });
    await shot(page, "25-chat-reply");
  });
});
