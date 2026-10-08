import { expect, test } from "@playwright/test";

test("users search is debounced and stays usable while results load", async ({ page }) => {
  const users = [
    { userId: 1, displayName: "Instance Owner", role: "owner", disabled: false },
    { userId: 2, displayName: "Alice", username: "alice", role: "user", disabled: false },
  ];
  const searches: (string | null)[] = [];
  let releaseSearch!: () => void;
  const searchResponse = new Promise<void>((resolve) => {
    releaseSearch = resolve;
  });

  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/v1/me") {
      return route.fulfill({
        json: {
          ...users[0],
          displayName: "Fixture Admin",
          capabilities: ["system.manageUsers", "system.owner"],
          premium: false,
          createdAt: "2026-07-22T12:00:00Z",
        },
      });
    }
    if (url.pathname === "/api/v1/admin/users") {
      const search = url.searchParams.get("search");
      searches.push(search);
      if (search) await searchResponse;
      return route.fulfill({
        json: search ? users.filter((user) => user.displayName.includes(search)) : users,
      });
    }
    return route.fulfill({ status: 404, json: {} });
  });

  // The fake clock is installed before the first navigation, and paused only
  // once the page has rendered. A timer the page schedules while it is loading is
  // a native one, and once the clock has replaced `clearTimeout` React can no
  // longer cancel it: a stale debounce timer would fire later and restore the
  // previous search term. Pausing before the first paint would instead stall the
  // notification that renders the initial list.
  await page.clock.install();
  await page.goto("/settings/users");
  await expect(page.getByText("Instance Owner", { exact: true })).toBeVisible();
  expect(searches).toEqual([null]);
  await page.clock.pauseAt(new Date(Date.now() + 1_000));

  const input = page.getByRole("textbox", { name: "Search users" });
  await input.fill("A");
  await page.clock.runFor(200);
  await input.fill("Al");
  await page.clock.runFor(200);
  await input.fill(" Alice ");
  await page.clock.runFor(299);
  expect(searches).toEqual([null]);
  await expect(input).toHaveValue(" Alice ");

  await page.clock.runFor(1);
  await expect.poll(() => searches).toEqual([null, "Alice"]);
  await expect(input).toBeVisible();
  await expect(input).toBeFocused();
  await expect(page.getByText("Instance Owner", { exact: true })).toBeVisible();

  // The response is still held here, so what the assertions above read is the
  // previous page of results: that is what keeps the field usable while the
  // search runs. The list is polled with the clock stepped, because the repaint
  // that follows the released response needs a timer tick of its own.
  releaseSearch();
  await expect
    .poll(async () => {
      await page.clock.runFor(50);
      return page.getByText("Instance Owner", { exact: true }).count();
    })
    .toBe(0);
  await expect(page.getByText("Alice", { exact: true })).toBeVisible();

  // Whitespace-only changes should not issue another search.
  await input.fill("Alice");
  await page.clock.runFor(300);
  expect(searches).toEqual([null, "Alice"]);

  // Clearing restores the cached unfiltered list without an unnecessary request.
  await input.fill("");
  await expect
    .poll(async () => {
      await page.clock.runFor(50);
      return page.getByText("Instance Owner", { exact: true }).count();
    })
    .toBe(1);
  expect(searches).toEqual([null, "Alice"]);
});
