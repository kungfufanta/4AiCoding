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

  test("invite page handles an unknown token", async ({ page }) => {
    await page.goto("/invites/00000000-0000-0000-0000-000000000000?token=not-a-real-token");
    await expect(page.getByText("Your invitation")).toBeVisible();
    await page.getByRole("button", { name: "Accept invite", exact: true }).click();
    await expect(
      page.getByText("This invite is not valid anymore — it expired or was already used."),
    ).toBeVisible();
    await shot(page, "09-invite-invalid");
  });
});
