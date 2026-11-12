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

  // A3.4 helpers: BFF calls ride the page's cookies (same-origin) and
  // must echo the CSRF cookie back (double-submit), so plain
  // page.request is not enough — evaluate inside the browser context.
  async function bff(page: Page, method: string, path: string, body?: unknown) {
    return page.evaluate(async ({ method, path, body }) => {
      const m = document.cookie.match(/(?:^|;\s*)portal_csrf=([^;]*)/);
      const res = await fetch(path, {
        method,
        headers: {
          "Content-Type": "application/json",
          ...(method !== "GET" && m ? { "X-CSRF-Token": decodeURIComponent(m[1]) } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      const text = await res.text();
      let data: unknown = null;
      try {
        data = JSON.parse(text);
      } catch {
        data = text;
      }
      return { status: res.status, data: data as Record<string, unknown> };
    }, { method, path, body });
  }

  // The agent e2e runs against the compose fake-llm (fixed script:
  // write_file → run_command → finish) and a git-capable sandbox image
  // (sandboxd-e2e-agent:ci) so checkpoint + revert really work.
  const FAKE_LLM_BASE = "http://fake-llm:9933/v1";

  test("agent mode — attach, run, result, revert (A3.4)", async ({ page }) => {
    test.setTimeout(240_000);
    const slug = await gotoOrgBase(page);

    // 0. Setup via BFF: the platform LLM connection (private compose
    //    URL → platform connections are exempt from the SSRF guard)
    //    and one git-capable sandbox, waited until running.
    const me = (await bff(page, "GET", "/api/me")).data as { orgs: { slug: string; id: string }[] };
    const orgID = me.orgs.find((o) => o.slug === slug)!.id;

    const conn = await bff(page, "POST", "/api/admin/llm/connections", {
      name: `fake-llm-${runStamp}`,
      protocol: "openai",
      flavor: "generic",
      base_url: FAKE_LLM_BASE,
      default_model: "fake-4.6",
      models: [{ id: "fake-4.6", supports_tools: true }],
      key_mode: "shared",
      timeout_s: 30,
      max_tokens: 4096,
      enabled: true,
      is_default: true,
      api_key: "sk_fake_local",
    });
    if (conn.status !== 201) throw new Error(`create connection = ${conn.status}: ${JSON.stringify(conn.data)}`);

    const sbx = await bff(page, "POST", `/api/orgs/${orgID}/sandboxes`, { image: "sandboxd-e2e-agent:ci" });
    if (sbx.status !== 201) throw new Error(`create sandbox = ${sbx.status}: ${JSON.stringify(sbx.data)}`);
    const sbxID = (sbx.data as { id: string }).id;
    let status = "";
    for (let i = 0; i < 60; i++) {
      const cur = await bff(page, "GET", `/api/orgs/${orgID}/sandboxes/${sbxID}`);
      status = (cur.data as { status?: string }).status ?? "";
      if (status === "running") break;
      await page.waitForTimeout(1000);
    }
    if (status !== "running") throw new Error(`sandbox never ran: ${status}`);

    // 1. Chat page: attach the sandbox through the header dropdown —
    //    this creates the conversation and navigates to it.
    await page.goto(`/orgs/${slug}/chat`);
    await page.getByLabel("Sandbox").click();
    await page.getByRole("option", { name: new RegExp(sbxID.slice(-6)) }).click();
    await expect(page).toHaveURL(new RegExp(`/orgs/${slug}/chat/[0-9a-f-]{36}$`), { timeout: 15_000 });
    await shot(page, "30-chat-attached");

    // 2. Agent mode on, hand over the fixed task.
    await page.getByRole("button", { name: "Agent", exact: true }).click();
    await page.getByPlaceholder(/Giao việc cho agent/).fill("Tạo file hello.txt cho tôi");
    await page.getByRole("button", { name: "Send" }).click();

    // 3. The prompt shows as an Agent bubble and the run card goes
    //    live; the fake LLM walks write_file → run_command → finish.
    const card = page.getByTestId("agent-run-card");
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("tool-row").filter({ hasText: "write_file" }).first()).toBeVisible({
      timeout: 90_000,
    });
    await shot(page, "31-agent-tool-write");
    await expect(page.getByTestId("tool-row").filter({ hasText: "run_command" }).first()).toBeVisible({
      timeout: 90_000,
    });
    await shot(page, "32-agent-tool-run");

    // 4. The result card: succeeded + summary + hello.txt changed.
    await expect(card).toHaveAttribute("data-status", "succeeded", { timeout: 120_000 });
    await expect(page.getByTestId("result-summary")).toContainText("hello.txt");
    // files_changed comes from the git checkpoint; if it is empty, dump
    // the run JSON + all events (tool_result carries exit_code and the
    // output preview; status carries the checkpoint ref) so the CI log
    // says WHY.
    const filesBox = page.getByTestId("files-changed");
    try {
      await expect(filesBox).toContainText("hello.txt", { timeout: 5_000 });
    } catch {
      const runID2 = (await page.getByTestId("agent-run-card").getAttribute("data-run-id")) ?? "";
      const runDump = await bff(page, "GET", `/api/orgs/${orgID}/agent/runs/${runID2}`);
      const evDump = await bff(page, "GET", `/api/orgs/${orgID}/agent/runs/${runID2}/events?follow=false`);
      throw new Error(
        `files-changed empty — run: ${JSON.stringify(runDump.data)}\nevents: ${JSON.stringify(evDump.data)}`,
      );
    }
    await shot(page, "33-agent-result");

    // 5. Revert through the UI confirm, then the files API proves the
    //    file is gone (plan §8 acceptance).
    await page.getByRole("button", { name: "Revert run" }).click();
    const confirmDialog = page.getByRole("dialog");
    await expect(confirmDialog.getByText("Revert run này?")).toBeVisible();
    await confirmDialog.getByRole("button", { name: "Revert", exact: true }).click();
    await expect(page.getByTestId("reverted-badge")).toBeVisible({ timeout: 60_000 });
    await shot(page, "34-agent-reverted");

    const key = await bff(page, "POST", `/api/orgs/${orgID}/api-keys`, {
      name: `agent-files-${runStamp}`,
      scopes: ["sandboxes:files"],
    });
    if (key.status !== 201) throw new Error(`create key = ${key.status}: ${JSON.stringify(key.data)}`);
    const secret = (key.data as { secret: string }).secret;
    const files = await page.evaluate(async ({ sbxID, secret }) => {
      const res = await fetch(`/v1/sandboxes/${sbxID}/files?path=workspace`, {
        headers: { Authorization: `Bearer ${secret}` },
      });
      return { status: res.status, body: await res.text() };
    }, { sbxID, secret });
    if (files.status !== 200) throw new Error(`files list = ${files.status}: ${files.body}`);
    if (files.body.includes("hello.txt")) {
      throw new Error(`hello.txt survived the revert: ${files.body}`);
    }
    await shot(page, "35-files-after-revert");
  });

  test("agent run survives a mid-run reload and Stop works (A3.4)", async ({ page }) => {
    test.setTimeout(240_000);
    const slug = await gotoOrgBase(page);

    // The previous test left a chat with a sandbox attached; open the
    // (still attached) chat via the sidebar, or attach a fresh sandbox
    // when the list is empty (rerun against a clean volume).
    await page.goto(`/orgs/${slug}/chat`);
    const me = (await bff(page, "GET", "/api/me")).data as { orgs: { slug: string; id: string }[] };
    const orgID = me.orgs.find((o) => o.slug === slug)!.id;

    const chats = (await bff(page, "GET", `/api/orgs/${orgID}/chats`)).data as
      | { id: string; sandbox_id: string }[]
      | null;
    let chatID = chats?.[0]?.id ?? "";
    let sbxID = chats?.[0]?.sandbox_id ?? "";
    if (!chatID || !sbxID) {
      const sbx = await bff(page, "POST", `/api/orgs/${orgID}/sandboxes`, { image: "sandboxd-e2e-agent:ci" });
      if (sbx.status !== 201) throw new Error(`create sandbox = ${sbx.status}: ${JSON.stringify(sbx.data)}`);
      sbxID = (sbx.data as { id: string }).id;
      chatID = ((await bff(page, "POST", `/api/orgs/${orgID}/chats`, {})).data as { id: string }).id;
      const att = await bff(page, "PATCH", `/api/orgs/${orgID}/chats/${chatID}`, { sandbox_id: sbxID });
      if (att.status !== 200) throw new Error(`attach = ${att.status}: ${JSON.stringify(att.data)}`);
    }

    // Fresh sandbox state: the previous test's run is on this chat;
    // work on a NEW chat so the run list starts empty for this test.
    const newChat = ((await bff(page, "POST", `/api/orgs/${orgID}/chats`, {})).data as { id: string }).id;
    const att = await bff(page, "PATCH", `/api/orgs/${orgID}/chats/${newChat}`, { sandbox_id: sbxID });
    if (att.status !== 200) throw new Error(`attach new chat = ${att.status}: ${JSON.stringify(att.data)}`);

    await page.goto(`/orgs/${slug}/chat/${newChat}`);
    await page.getByRole("button", { name: "Agent", exact: true }).click();

    // --- reload mid-run -------------------------------------------------
    await page.getByPlaceholder(/Giao việc cho agent/).fill("Chạy lại kịch bản hello");
    await page.getByRole("button", { name: "Send" }).click();
    const card = page.getByTestId("agent-run-card");
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("tool-row").first()).toBeVisible({ timeout: 90_000 });

    await page.reload();
    // The run card rebuilds from GET runs + SSE replay (seq 0): the
    // already-seen tool row reappears and the run still finishes.
    await expect(page.getByTestId("agent-run-card")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("tool-row").first()).toBeVisible({ timeout: 90_000 });
    await expect(page.getByTestId("agent-run-card")).toHaveAttribute("data-status", "succeeded", {
      timeout: 120_000,
    });
    await shot(page, "36-agent-reload-midrun");

    // --- Stop mid-run ---------------------------------------------------
    const stopped = ((await bff(page, "POST", `/api/orgs/${orgID}/chats`, {})).data as { id: string }).id;
    const att2 = await bff(page, "PATCH", `/api/orgs/${orgID}/chats/${stopped}`, { sandbox_id: sbxID });
    if (att2.status !== 200) throw new Error(`attach stop chat = ${att2.status}: ${JSON.stringify(att2.data)}`);
    await page.goto(`/orgs/${slug}/chat/${stopped}`);
    await page.getByRole("button", { name: "Agent", exact: true }).click();
    await page.getByPlaceholder(/Giao việc cho agent/).fill("Run này sẽ bị dừng giữa chừng");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByTestId("agent-run-card")).toBeVisible({ timeout: 30_000 });
    await page.getByRole("button", { name: "Dừng run" }).click();
    await expect(page.getByTestId("agent-run-card")).toHaveAttribute("data-status", "cancelled", {
      timeout: 30_000,
    });
    await shot(page, "37-agent-stopped");

    // --- dark mode + mobile 390 px of the agent view --------------------
    await page.emulateMedia({ colorScheme: "dark" });
    await page.evaluate(() => document.documentElement.classList.add("dark"));
    await shot(page, "38-agent-dark");
    await page.setViewportSize({ width: 390, height: 844 });
    await shot(page, "39-agent-mobile-390");
    await page.setViewportSize({ width: 1280, height: 720 });
  });
});
