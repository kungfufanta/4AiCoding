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
  // Z1 #83: the submit button is Vietnamese now.
  await page.getByRole("button", { name: "Đăng nhập", exact: true }).click();

  // Z1 #81: sign-in lands straight in the first org's Chat.
  await expect(page).toHaveURL(/\/orgs\/.+\/chat/, { timeout: 15_000 });
  await page.context().storageState({ path: authFile });
});
