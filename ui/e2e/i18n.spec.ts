import { expect, type Page, test } from "@playwright/test";

// The fixed instant the profile fixture is timestamped with.
const now = "2026-07-22T12:00:00Z";

/**
 * Answers the requests the application shell makes on load: the signed-in user
 * for the root route guard and an empty listing for anything else, so the
 * appearance page renders without a backend.
 */
async function installShellApi(page: Page) {
  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname.replace(/^\/api/, "");

    if (path === "/v1/me") {
      return route.fulfill({
        json: {
          userId: 1,
          displayName: "Fixture User",
          username: "fixture",
          premium: true,
          role: "owner",
          capabilities: [
            "files.read",
            "files.write",
            "files.share",
            "system.manageUsers",
            "system.manageJobs",
            "system.manageQueues",
            "system.localImport",
            "system.maintenance",
            "system.owner",
          ],
          createdAt: now,
        },
      });
    }

    return route.fulfill({ json: { items: [] } });
  });
}

test("the language switcher reloads the interface in Chinese and remembers it", async ({ page }) => {
  await installShellApi(page);
  await page.goto("/settings/appearance");

  await expect(page.getByRole("heading", { name: "Appearance", level: 1 })).toBeVisible();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");

  await page.getByRole("button", { name: "简体中文" }).click();
  await expect(page.getByRole("heading", { name: "外观", level: 1 })).toBeVisible();
  await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN");

  // The choice survives a reload, which is what makes it a setting rather than a
  // per-visit toggle.
  await page.reload();
  await expect(page.getByRole("heading", { name: "外观", level: 1 })).toBeVisible();

  await page.getByRole("button", { name: "English" }).click();
  await expect(page.getByRole("heading", { name: "Appearance", level: 1 })).toBeVisible();
});

test.describe("a browser asking for Chinese", () => {
  test.use({ locale: "zh-CN" });

  test("opens the interface in Chinese without a choice", async ({ page }) => {
    await installShellApi(page);
    await page.goto("/settings/appearance");

    await expect(page.getByRole("heading", { name: "外观", level: 1 })).toBeVisible();
    await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN");
  });
});
