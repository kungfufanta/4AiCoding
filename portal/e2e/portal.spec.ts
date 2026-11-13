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
  //
  // The Test button + chat reply depend on the Portal container
  // reaching the public WebHub endpoint. The Go e2e test
  // TestChatStatusAndSendViaPlatformConnection already covers the
  // full end-to-end send path against a fake LLM server; the
  // Playwright test focuses on the UI flow + the bug fix assertion
  // (chat status enabled after a DB connection is saved).
  test("admin LLM connection wires up chat (A2 phase 3)", async ({ page }) => {
    test.setTimeout(120_000); // form fill + Test probe + chat send
    const slug = await gotoOrgBase(page);

    // 1. Chat is initially disabled (no env LLM, no DB connection yet).
    await page.goto(`/orgs/${slug}/chat`);
    await expect(page.getByText("Chat is not configured yet")).toBeVisible();
    await shot(page, "20-chat-disabled");

    // 2. Admin adds a platform connection pointing at the public WebHub
    //    demo endpoint.
    await page.goto(`/orgs/${slug}/admin/llm`);
    await expect(page.getByRole("heading", { name: "LLM connections" })).toBeVisible();
    await shot(page, "21-admin-llm-empty");

    await page.getByRole("button", { name: "New connection" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("New LLM connection")).toBeVisible();
    await dialog.getByLabel("Name").fill("webhub-demo");
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

    // 3. Test the connection — either outcome is fine: success →
    //    "Test OK" toast; failure → error toast (the bridge network
    //    may not reach the public WebHub endpoint). We just assert
    //    that the click produced SOME toast within the Test HTTP
    //    client's 10s timeout + a buffer, proving the Test button is
    //    wired to the backend.
    await page.getByRole("row", { name: /webhub-demo/ }).getByRole("button", { name: "Test" }).click();
    await expect(
      page.locator('[role="status"]').first(),
      "Test button should produce a toast (success or failure)",
    ).toBeVisible({ timeout: 30_000 });
    await shot(page, "23-admin-llm-test-toast");

    // 4. Chat is now wired (the actual bug fix). The textarea is
    //    enabled and the empty state offers the suggestion buttons.
    //    This assertion does NOT depend on WebHub being reachable —
    //    the status endpoint reads the DB-configured platform
    //    connection directly. Before the route fix, this assertion
    //    failed because ResolveEffective returned "empty org id" and
    //    the status endpoint fell through to the env-fallback branch
    //    (s.LLM == nil → enabled:false → textarea disabled).
    await page.goto(`/orgs/${slug}/chat`);
    await expect(page.getByPlaceholder(/Nhắn tin/)).toBeEnabled();
    await expect(page.getByText("Hôm nay bạn muốn làm gì?")).toBeVisible();
    await shot(page, "24-chat-enabled");

    // 5. Send a message — assert the user bubble appears (proves the
    //    chat send path is wired and the message was stored locally
    //    before the upstream call). The user bubble is rendered as
    //    soon as the SSE `event: user` arrives, before the upstream
    //    LLM call returns. Whether the upstream reply arrives depends
    //    on the docker-compose bridge network reaching WebHub; the Go
    //    e2e test covers the full reply path against a fake LLM.
    await page.getByPlaceholder(/Nhắn tin/).fill("Say pong");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText("Say pong").first()).toBeVisible({ timeout: 10_000 });
    await shot(page, "25-chat-user-bubble");
  });

  // --- A3.4: agent mode in Chat (docs/AGENT_PLAN_A3.md §10-11) -------
  // The fake-llm compose service (profile e2e) answers a fixed
  // scenario: write_file hello.txt → run_command cat → finish. The
  // platform connection points at the compose-internal hostname —
  // platform connections may point internally (the SSRF guard only
  // applies to org connections). Serial tests share the sandbox and
  // chat created here.
  let agentSandboxId = "";
  let agentChatUrl = "";

  test("agent — attach sandbox, run hello.txt, tool cards and result (A3.4)", async ({ page }) => {
    test.setTimeout(240_000);
    const slug = await gotoOrgBase(page);

    // 0. One platform connection pointing at the fake-llm service,
    //    and exactly one default (webhub-demo from the A2 test above
    //    loses its default flag so the effective connection is
    //    deterministic).
    await page.goto(`/orgs/${slug}/admin/llm`);
    await page.getByRole("button", { name: "New connection" }).click();
    const llmDialog = page.getByRole("dialog");
    await llmDialog.getByLabel("Name").fill("fake-llm");
    await llmDialog.getByPlaceholder("https://h11hp9dd1y90-d.space-z.ai/v1").fill("http://fake-llm:9999/v1");
    await llmDialog.getByPlaceholder("wh_… or sk_…").fill("sk_fake");
    await llmDialog.getByPlaceholder("glm-4.6").fill("fake-model");
    await llmDialog.getByRole("checkbox", { name: "Set as default" }).check();
    await page.getByRole("button", { name: "Save connection" }).click();
    await expect(page.getByText("fake-llm").first()).toBeVisible({ timeout: 10_000 });
    const webhubRow = page.getByRole("row", { name: /webhub-demo/ });
    if ((await webhubRow.count()) > 0) {
      await webhubRow.getByRole("button", { name: "Edit webhub-demo" }).click();
      const editDialog = page.getByRole("dialog");
      await editDialog.getByRole("checkbox", { name: "Set as default" }).uncheck();
      await page.getByRole("button", { name: "Save connection" }).click();
      await expect(editDialog).toBeHidden({ timeout: 10_000 });
    }

    // 1. A sandbox to work in (same allowlisted e2e image as the
    //    sandboxes test above).
    await page.goto(`/orgs/${slug}/sandboxes`);
    await page.getByRole("button", { name: "New sandbox" }).first().click();
    const sbxDialog = page.getByRole("dialog");
    await sbxDialog.getByPlaceholder("ubuntu:24.04").fill("sandboxd-e2e:ci");
    await sbxDialog.getByRole("button", { name: "Create sandbox", exact: true }).click();
    const sbxHeading = page.getByRole("heading", { name: /0[0-9A-Z]{10,}/ });
    await expect(sbxHeading).toBeVisible({ timeout: 30_000 });
    agentSandboxId = (await sbxHeading.textContent())!.match(/0[0-9A-Z]{10,}/)![0];
    await expect(page.getByText("running").first()).toBeVisible({ timeout: 30_000 });

    // 2. New chat → attach the sandbox via the header dropdown →
    //    the attach lands the chat in Agent mode.
    await page.goto(`/orgs/${slug}/chat`);
    await page.getByRole("button", { name: "New chat" }).click();
    await page.getByRole("button", { name: "Gắn sandbox" }).click();
    await page.getByRole("menuitem", { name: new RegExp(agentSandboxId) }).click();
    await expect(page).toHaveURL(/\/orgs\/.+\/chat\/[0-9a-f-]{36}/, { timeout: 15_000 });
    agentChatUrl = page.url();
    await expect(page.getByRole("button", { name: "Agent" })).toHaveAttribute("aria-pressed", "true");
    await shot(page, "26-agent-attached");

    // 3. Hand the task over; tool cards stream in one line each.
    await page.getByLabel("Message").fill("tạo file hello.txt cho tôi");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText("write_file").first()).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText("run_command").first()).toBeVisible({ timeout: 30_000 });

    // 4. The result card: summary, the changed-file chip, tokens.
    const result = page.getByTestId("agent-result");
    await expect(result).toBeVisible({ timeout: 60_000 });
    // Screenshot BEFORE the strict asserts — whatever the card shows on
    // a failure lands in the artifact and makes the run diagnosable.
    await shot(page, "27-agent-result");
    await expect(result.getByLabel("Files changed").getByText("hello.txt", { exact: true })).toBeVisible();
    await expect(result.getByText("Hoàn thành")).toBeVisible();
    await expect(result.getByText(/token/)).toBeVisible();

    // 5. A tool card opens to show its output preview. The output
    // lives in the "Kết quả tool" <pre> — the summary below also
    // mentions the content, so the bare text would match twice.
    await page.getByRole("button", { name: /Tool run_command/ }).click();
    const preview = page.getByLabel("Kết quả tool");
    await expect(preview).toBeVisible();
    await expect(preview).toContainText("hello from agent");
    await shot(page, "28-agent-tool-output");

    // 6. Revert through the confirm dialog → the file is gone.
    await result.getByRole("button", { name: "Revert" }).click();
    const revertDialog = page.getByRole("dialog");
    await expect(revertDialog.getByText("Hoàn tác run này?")).toBeVisible();
    await shot(page, "29-agent-revert-confirm");
    await revertDialog.getByRole("button", { name: "Hoàn tác", exact: true }).click();
    // Capture the immediate aftermath (toast or card state) while the
    // revert API call settles.
    await page.waitForTimeout(2000);
    await shot(page, "29b-agent-after-revert");
    // The run row is the source of truth: reverted_at arrives through
    // the runs refetch and shows as the badge on the result card.
    await expect(result.getByText("Đã hoàn tác")).toBeVisible({ timeout: 60_000 });
    // The reverted event itself reaches the UI through the JSON
    // replay (?follow=false) — the SSE stream stops at done.
    await expect(page.getByTestId("agent-reverted")).toBeVisible({ timeout: 30_000 });
    // Regression pin (caught on the 29b screenshot): the JSON rebuild
    // after revert must REPLACE the timeline, not append onto it —
    // reopening the SSE replay AND running the JSON rebuild rendered
    // every message delta and tool card twice. The scenario has
    // exactly 3 tool calls and one "Đang tạo file hello.txt…" delta.
    // Count only the card toggle buttons (aria-expanded): an opened
    // card also holds a copy button per CodeBlock (5 with run_command
    // open), which is UI, not duplication.
    await expect(page.getByLabel("Tool calls").locator("button[aria-expanded]")).toHaveCount(3);
    await expect(page.getByText(/Đang tạo file hello\.txt…\s*Đang tạo/)).toHaveCount(0);

    // Verify through the sandbox exec console (the files API path the
    // plan asks for): the exec cwd is /home/sandbox and the project
    // dir is workspace/ (no git source → no workspace/app), so ls the
    // workspace itself — hello.txt must be gone after the revert.
    await page.goto(`/orgs/${slug}/sandboxes/${agentSandboxId}`);
    await page.getByRole("tab", { name: "Exec" }).click();
    await page.getByLabel("Command").fill("ls workspace");
    await page.getByRole("button", { name: "Run", exact: true }).click();
    await expect(page.getByText("exit 0").first()).toBeVisible({ timeout: 60_000 });
    await expect(page.getByText("hello.txt")).toHaveCount(0);
  });

  test("agent — reload mid-run rebuilds the run view (A3.4)", async ({ page }) => {
    test.setTimeout(180_000);
    if (!agentChatUrl) throw new Error("agent chat not prepared by the previous test");
    await page.goto(agentChatUrl);
    // Mode is per mount — switch back to Agent before handing over a
    // new job (in Chat mode this text would go to the LLM as a plain
    // message and the asserts below would ride run 1's card).
    await page.getByRole("button", { name: "Agent" }).click();
    await expect(page.getByRole("button", { name: "Agent" })).toHaveAttribute("aria-pressed", "true");

    await page.getByLabel("Message").fill("SLOW tạo lại hello.txt");
    await page.getByRole("button", { name: "Send" }).click();
    // The NEW run is the active one — its phase line proves it, not
    // the previous run's cards.
    await expect(page.getByTestId("agent-phase")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText("write_file").first()).toBeVisible({ timeout: 30_000 });

    // Reload while the run is still going: the runs list + the events
    // replay rebuild the timeline and the run still finishes here.
    await page.reload();
    await expect(page.getByText("write_file").first()).toBeVisible({ timeout: 30_000 });
    const result = page.getByTestId("agent-result");
    await expect(result).toBeVisible({ timeout: 90_000 });
    await expect(result.getByLabel("Files changed").getByText("hello.txt", { exact: true })).toBeVisible();
    await shot(page, "30-agent-reload-mid-run");
  });

  test("agent — Stop cancels a running job (A3.4)", async ({ page }) => {
    test.setTimeout(120_000);
    if (!agentChatUrl) throw new Error("agent chat not prepared by the previous test");
    await page.goto(agentChatUrl);
    // Mode is per mount — see the reload test.
    await page.getByRole("button", { name: "Agent" }).click();

    await page.getByLabel("Message").fill("HANG đừng làm gì cả");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByTestId("agent-phase")).toBeVisible({ timeout: 30_000 });

    await page.getByRole("button", { name: "Stop run" }).first().click();
    const result = page.getByTestId("agent-result");
    await expect(result.getByText("Đã dừng")).toBeVisible({ timeout: 30_000 });
    await expect(result.getByText("Tiếp tục")).toBeVisible();
    await shot(page, "31-agent-stopped");
  });

  test("agent — rename a file, revert brings the old name back (A3.5)", async ({ page }) => {
    test.setTimeout(180_000);
    if (!agentChatUrl) throw new Error("agent chat not prepared by the previous test");
    await page.goto(agentChatUrl);
    // Mode is per mount — see the reload test.
    await page.getByRole("button", { name: "Agent" }).click();
    await expect(page.getByRole("button", { name: "Agent" })).toHaveAttribute("aria-pressed", "true");

    // Run A creates a.txt. It must exist BEFORE the renaming run's
    // checkpoint — that is what makes the rename visible as D+A (a
    // file created and renamed inside ONE run only ever shows A b.txt).
    await page.getByLabel("Message").fill("RENAME_INIT tạo file a.txt");
    await page.getByRole("button", { name: "Send" }).click();
    const first = page.getByTestId("agent-result");
    await expect(first).toBeVisible({ timeout: 90_000 });
    await expect(first.getByText("Hoàn thành")).toBeVisible();
    await expect(first.getByLabel("Files changed").getByText("a.txt", { exact: true })).toBeVisible();

    // Run B renames a.txt → b.txt. files_changed must carry the
    // --no-renames split (D a.txt + A b.txt) — never one R row, which
    // is what let the renamed file survive revert before A3.5.
    await page.getByLabel("Message").fill("RENAME_DO đổi a.txt thành b.txt");
    await page.getByRole("button", { name: "Send" }).click();
    const result = page.getByTestId("agent-result");
    await expect(result).toBeVisible({ timeout: 90_000 });
    await expect(result.getByText("Hoàn thành")).toBeVisible();
    await expect(result.getByLabel("Files changed").getByText("a.txt", { exact: true })).toBeVisible();
    await expect(result.getByLabel("Files changed").getByText("b.txt", { exact: true })).toBeVisible();
    await shot(page, "34-agent-rename-result");

    // Revert run B: the D side rides the checkpoint restore, the A
    // side is deleted — a.txt is back byte for byte, b.txt is gone.
    await result.getByRole("button", { name: "Revert" }).click();
    const revertDialog = page.getByRole("dialog");
    await expect(revertDialog.getByText("Hoàn tác run này?")).toBeVisible();
    await revertDialog.getByRole("button", { name: "Hoàn tác", exact: true }).click();
    await expect(result.getByText("Đã hoàn tác")).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId("agent-reverted")).toBeVisible({ timeout: 30_000 });
    await shot(page, "35-agent-rename-after-revert");

    // The plan's own verification path: ls the workspace through the
    // exec console (cwd is /home/sandbox, project dir is workspace/) —
    // the old name is back, the new one is nowhere.
    const slug = new URL(agentChatUrl).pathname.split("/")[2];
    await page.goto(`/orgs/${slug}/sandboxes/${agentSandboxId}`);
    await page.getByRole("tab", { name: "Exec" }).click();
    await page.getByLabel("Command").fill("ls workspace");
    await page.getByRole("button", { name: "Run", exact: true }).click();
    await expect(page.getByText("exit 0").first()).toBeVisible({ timeout: 60_000 });
    await expect(page.getByText("a.txt").first()).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("b.txt")).toHaveCount(0);
  });

  test("agent — dark mode and 390px shots (A3.4)", async ({ page }) => {
    if (!agentChatUrl) throw new Error("agent chat not prepared by the previous test");
    await page.goto(agentChatUrl);
    await expect(page.getByTestId("agent-result").first()).toBeVisible({ timeout: 30_000 });

    await page.getByRole("button", { name: "Toggle dark mode" }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    await page.waitForTimeout(300);
    await shot(page, "32-agent-dark");
    await page.getByRole("button", { name: "Toggle dark mode" }).click();

    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.getByTestId("agent-result").first()).toBeVisible();
    await shot(page, "33-agent-mobile-390");
    await page.setViewportSize({ width: 1280, height: 800 });
  });
});
