import { expect, type Page, test } from "@playwright/test";

const now = "2026-07-22T12:00:00Z";

/**
 * Answers the requests the application shell makes on load: the signed-in user
 * for the root route guard and an empty listing for anything else, so the
 * appearance page and the file browser render without a backend.
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
          capabilities: ["files.read", "files.write", "system.owner"],
          createdAt: now,
        },
      });
    }

    return route.fulfill({ json: { items: [] } });
  });
}

/** The theme is applied to the document element as a class. */
function documentTheme(page: Page) {
  return page.locator("html");
}

/**
 * The colour-theme choice on the appearance page. Matching the accessible name
 * exactly keeps the toolbar button ("Switch to the light theme") out of the
 * result while the two carry similar names.
 */
function themeChoice(page: Page, name: string) {
  return page.getByRole("button", { name, exact: true });
}

test("follows the system preference until a choice is made", async ({ page }) => {
  await installShellApi(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/settings/appearance");

  await expect(documentTheme(page)).toHaveClass(/light/);
  await expect(themeChoice(page, "System")).toHaveAttribute("aria-pressed", "true");

  // A browser that switches its system preference flips the interface with it,
  // which is what makes "system" a live mode rather than a snapshot.
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(documentTheme(page)).toHaveClass(/dark/);
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
});

test("a pinned theme wins over the system preference and survives a reload", async ({ page }) => {
  await installShellApi(page);
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto("/settings/appearance");

  await themeChoice(page, "Light").click();
  await expect(documentTheme(page)).toHaveClass(/light/);
  await expect(themeChoice(page, "Light")).toHaveAttribute("aria-pressed", "true");

  await page.reload();
  await expect(documentTheme(page)).toHaveClass(/light/);
  await expect(themeChoice(page, "Light")).toHaveAttribute("aria-pressed", "true");
  // color-scheme drives native surfaces (form controls, scrollbars), so it has to
  // follow the pinned theme rather than the system preference.
  await expect(documentTheme(page)).toHaveCSS("color-scheme", "light");

  // Handing control back to the system preference resolves it immediately.
  await themeChoice(page, "System").click();
  await expect(documentTheme(page)).toHaveClass(/dark/);
});

test("the toolbar button switches to the opposite theme", async ({ page }) => {
  await installShellApi(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/files");

  await expect(documentTheme(page)).toHaveClass(/light/);
  // The file browser has to render, not just the shell: a response the UI
  // rejects as incompatible used to leave only the theme assertions passing.
  await expect(page.getByRole("button", { name: "Grid view" })).toBeVisible();
  await expect(page.getByText("This folder is empty", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "Switch to the dark theme" }).click();
  await expect(documentTheme(page)).toHaveClass(/dark/);

  await page.getByRole("button", { name: "Switch to the light theme" }).click();
  await expect(documentTheme(page)).toHaveClass(/light/);
});
