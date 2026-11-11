// Signs the bootstrap admin in once; every later test reuses the
// storage state. The password comes from the compose environment —
// since P0 there is no default admin password.

import { expect, test as setup } from "@playwright/test";

const authFile = "playwright/.auth/admin.json";

const username = process.env.PORTAL_ADMIN_USERNAME ?? "admin";
const password = process.env.PORTAL_ADMIN_PASSWORD ?? "";

setup("sign in as the bootstrap admin", async ({ page }) => {
  await page.goto("/login");
  await page.getByPlaceholder("admin").fill(username);
  await page.locator('input[type="password"]').fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();

  await expect(page).toHaveURL(/\/orgs\/.+\/projects/, { timeout: 15_000 });
  await page.context().storageState({ path: authFile });
});
