// Portal preview browser tests (PORTAL_PLAN_V2 P1 "Xong khi"): drive
// the real compose stack (postgres + portal with the bootstrap admin)
// through login, projects, members/invite, audit, dark mode and the
// mobile layout, saving one screenshot per screen for the CI artifact.

import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  testMatch: /(auth\.setup\.ts|portal\.spec\.ts)/,
  timeout: 30_000,
  retries: 0,
  // One bootstrap admin; the tests share signed-in state and run serially.
  workers: 1,
  fullyParallel: false,
  outputDir: "test-results/run",
  use: {
    baseURL: process.env.BASE_URL ?? "http://localhost:8080",
    screenshot: "off",
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "setup",
      testMatch: /auth\.setup\.ts/,
    },
    {
      name: "portal",
      testMatch: /portal\.spec\.ts/,
      use: { ...devices["Desktop Chrome"] },
      dependencies: ["setup"],
      storageState: "playwright/.auth/admin.json",
    },
  ],
});
