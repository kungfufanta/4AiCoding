// Issue #87: the main product journey on the preview stack, starting
// signed out: landing → "Bắt đầu chat miễn phí" → /login → password
// sign-in → Chat → "xin chào" → the fake-llm reply shows up. Runs on a
// desktop viewport and again at 390×844 (phone), with screenshots of
// landing, login and chat in test-results/preview and a check that the
// page never scrolls sideways.
//
// Order: this project runs after the "portal" project (playwright
// config dependencies) because it makes the fake-llm connection the
// platform default, and portal.spec.ts first asserts that Chat starts
// unconfigured. The test sets up everything it needs itself.

import { expect, test, type Page } from "@playwright/test";

const SHOTS = "test-results/preview";
const username = process.env.PORTAL_ADMIN_USERNAME ?? "admin";
const password = process.env.PORTAL_ADMIN_PASSWORD ?? "";

async function noSideScroll(page: Page) {
  const { scrollWidth, innerWidth } = await page.evaluate(() => ({
    scrollWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth,
  }));
  expect(scrollWidth, "page must not scroll sideways").toBeLessThanOrEqual(innerWidth);
}

// Make the compose-internal fake-llm the default platform connection
// (creating a default clears the others' default flag server-side).
async function ensureFakeLLMDefault(page: Page) {
  const status = await page.evaluate(async () => {
    const csrf = decodeURIComponent(document.cookie.match(/(?:^|;\s*)portal_csrf=([^;]*)/)?.[1] ?? "");
    const list = await fetch("/api/admin/llm/connections", { credentials: "same-origin" });
    if (!list.ok) return `list ${list.status}`;
    const { connections } = (await list.json()) as {
      connections: { base_url: string; is_default: boolean; enabled: boolean }[];
    };
    if (connections.some((c) => c.is_default && c.enabled && c.base_url.startsWith("http://fake-llm:9999"))) {
      return "ok";
    }
    const res = await fetch("/api/admin/llm/connections", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify({
        name: `fake-llm-journey-${Date.now()}`,
        protocol: "openai",
        flavor: "generic",
        base_url: "http://fake-llm:9999/v1",
        default_model: "fake-model",
        models: [],
        key_mode: "shared",
        timeout_s: 60,
        max_tokens: 1024,
        enabled: true,
        is_default: true,
        api_key: "sk_fake",
      }),
    });
    return res.ok ? "ok" : `create ${res.status}: ${await res.text()}`;
  });
  expect(status).toBe("ok");
}

for (const vp of [
  { name: "desktop", width: 1280, height: 800 },
  { name: "mobile", width: 390, height: 844 },
]) {
  test(`journey (${vp.name}): landing → sign in → chat reply`, async ({ page }) => {
    test.setTimeout(90_000);
    await page.setViewportSize({ width: vp.width, height: vp.height });

    // 1. Landing, signed out.
    await page.goto("/");
    const cta = page.getByRole("link", { name: "Bắt đầu chat miễn phí" }).first();
    await expect(cta).toBeVisible();
    await noSideScroll(page);
    await page.screenshot({ path: `${SHOTS}/journey-${vp.name}-1-landing.png`, fullPage: true });

    // 2. CTA → /login (the preview stack has no SSO, so signup is off).
    await cta.click();
    await expect(page).toHaveURL(/\/login/);
    await expect(page.getByPlaceholder("admin")).toBeVisible();
    await noSideScroll(page);
    await page.screenshot({ path: `${SHOTS}/journey-${vp.name}-2-login.png`, fullPage: true });

    // 3. Password sign-in → straight into the first org's Chat.
    await page.getByPlaceholder("admin").fill(username);
    await page.locator('input[type="password"]').fill(password);
    await page.getByRole("button", { name: "Đăng nhập", exact: true }).click();
    await expect(page).toHaveURL(/\/orgs\/.+\/chat/, { timeout: 15_000 });

    // 4. A working model, then "xin chào" → the fake-llm reply.
    await ensureFakeLLMDefault(page);
    await page.reload();
    const box = page.getByPlaceholder(/Nhắn tin/);
    await expect(box).toBeEnabled({ timeout: 15_000 });
    await box.fill("xin chào");
    await page.getByRole("button", { name: "Send" }).click();
    // :visible — on a phone the conversation list (holding the desktop
    // run's "xin chào" chat title) is a hidden drawer.
    await expect(page.locator(':text-is("xin chào"):visible').first()).toBeVisible({ timeout: 10_000 });
    await expect(page.locator(':text("pong — fake-llm."):visible').first()).toBeVisible({ timeout: 30_000 });
    await noSideScroll(page);
    await page.screenshot({ path: `${SHOTS}/journey-${vp.name}-3-chat.png`, fullPage: true });
  });
}
