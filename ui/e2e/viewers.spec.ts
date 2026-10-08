import { readFileSync } from "node:fs";
import { expect, type Page, test } from "@playwright/test";
import { strToU8, zipSync } from "fflate";

const now = "2026-08-01T12:00:00Z";
const pdfId = "71111111-1111-4111-8111-111111111111";
const epubId = "72222222-2222-4222-8222-222222222222";
const longPdfId = "73333333-3333-4333-8333-333333333333";
const pdf = readFileSync(new URL("./fixtures/viewers/sample.pdf", import.meta.url));
const longPdf = readFileSync(new URL("./fixtures/viewers/sample-long.pdf", import.meta.url));
const epub = makeEpub();

// foliate-view is a custom element: the DOM types only know it as an element,
// while its instance carries the parsed book and the active renderer, which is
// what these assertions read from inside the page.
type FoliateView = HTMLElement & {
  book?: { metadata?: { title?: string }; sections?: unknown[] };
  renderer?: Element | null;
};

const files = [
  file(pdfId, "reader-sample.pdf", "application/pdf", pdf.byteLength),
  file(epubId, "reader-sample.epub", "application/epub+zip", epub.byteLength),
  file(longPdfId, "reader-long.pdf", "application/pdf", longPdf.byteLength),
];

type ViewerApiStats = { pdfContentRequests: number };

function file(id: string, name: string, mimeType: string, size: number) {
  return {
    id,
    name,
    kind: "file",
    status: "active",
    generation: 1,
    mimeType,
    size,
    modTime: now,
    createdAt: now,
    updatedAt: now,
    encryption: true,
  };
}

async function settleBrowserLayout(page: Page) {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      }),
  );
}

function makeEpub() {
  const fixture = (path: string) =>
    readFileSync(new URL(`./fixtures/viewers/epub-src/${path}`, import.meta.url));
  return Buffer.from(
    zipSync({
      mimetype: [strToU8("application/epub+zip"), { level: 0 }],
      "META-INF/container.xml": fixture("META-INF/container.xml"),
      "OEBPS/content.opf": fixture("OEBPS/content.opf"),
      "OEBPS/nav.xhtml": fixture("OEBPS/nav.xhtml"),
      "OEBPS/chapter-one.xhtml": fixture("OEBPS/chapter-one.xhtml"),
      "OEBPS/chapter-two.xhtml": fixture("OEBPS/chapter-two.xhtml"),
    }),
  );
}

async function installViewerApi(page: Page, stats?: ViewerApiStats) {
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace(/^\/api/, "");
    const method = request.method();
    if (path === "/v1/me") {
      return route.fulfill({
        json: {
          userId: 1,
          displayName: "Reader",
          premium: true,
          role: "user",
          capabilities: ["files.read", "files.write", "files.share"],
          createdAt: now,
        },
      });
    }
    if (path === "/v1/files" && method === "GET") {
      return route.fulfill({ json: { items: files } });
    }
    if (path === "/v1/files/statistics/drive") {
      return route.fulfill({
        json: {
          totalFiles: 2,
          totalFolders: 0,
          totalBytes: pdf.byteLength + epub.byteLength,
          trashedFiles: 0,
          activeShares: 0,
          openUploads: 0,
        },
      });
    }
    const content = path.match(/^\/v1\/files\/([^/]+)\/content(?:\/[^/]+)?$/)?.[1];
    if (content === pdfId) {
      if (stats) stats.pdfContentRequests += 1;
      return route.fulfill({ body: pdf, contentType: "application/pdf" });
    }
    if (content === longPdfId) {
      return route.fulfill({ body: longPdf, contentType: "application/pdf" });
    }
    if (content === epubId) {
      return route.fulfill({ body: epub, contentType: "application/epub+zip" });
    }
    return route.fulfill({
      status: 404,
      json: { error: { code: "not_found", message: `${method} ${path}` } },
    });
  });
}

async function openFile(page: Page, name: string) {
  const row = page.getByRole("row", { name: new RegExp(name) });
  await expect(row).toBeVisible();
  await row.focus();
  await page.keyboard.press("Enter");
}

test("PDF opens in the Teldrive PDF.js workspace with navigation and search", async ({ page }) => {
  const errors: string[] = [];
  const stats: ViewerApiStats = { pdfContentRequests: 0 };
  page.on("pageerror", (error) => errors.push(error.message));
  await installViewerApi(page, stats);
  await page.goto("/files?view=list");
  await openFile(page, "reader-sample.pdf");

  const dialog = page.getByRole("dialog", { name: "reader-sample.pdf" });
  await expect(dialog).toBeVisible();
  await expect(dialog.locator("[data-pdf-reader]")).toBeVisible();
  await expect(dialog.locator("foliate-view")).toHaveCount(0);
  // The first render boots the pdf.js worker and its wasm bundle before the
  // pages exist. That is slow enough on a fully parallel, four-core run to miss
  // the five second default, and a missing page is still a failure: the poll
  // only waits longer for the same two pages.
  await expect
    .poll(() => dialog.locator(".pdfViewer .page").count(), { timeout: 20_000 })
    .toBe(2);
  await expect(dialog.locator(".pdfViewer .textLayer").first()).toBeVisible();
  const initialContentRequests = stats.pdfContentRequests;

  const pageInput = dialog.getByRole("textbox", { name: "PDF page number" });
  await expect(pageInput).toHaveValue("1");
  await dialog.getByRole("button", { name: "Next PDF page" }).click();
  await expect(pageInput).toHaveValue("2");
  // The zoom shortcut has to scale the rendered page. Reader state is no longer
  // persisted, so the observable effect is the page geometry itself.
  const renderedPage = dialog.locator(".pdfViewer .page").first();
  const pageWidth = (await renderedPage.boundingBox())?.width ?? 0;
  expect(pageWidth).toBeGreaterThan(0);
  await page.keyboard.press("=");
  await expect
    .poll(async () => (await renderedPage.boundingBox())?.width ?? 0, { timeout: 5_000 })
    .toBeGreaterThan(pageWidth);
  expect(stats.pdfContentRequests).toBe(initialContentRequests);

  const viewportWidth = page.viewportSize()?.width ?? 0;
  if (viewportWidth >= 1024) {
    await expect(dialog.getByRole("button", { name: "Go to page 2" })).toBeVisible();
  } else {
    await dialog.getByRole("button", { name: "Open PDF sidebar" }).click();
    const navigation = page.getByRole("dialog", { name: "Document navigation" });
    await expect(navigation).toBeVisible();
    await expect(navigation.getByRole("button", { name: "Go to page 2" })).toBeVisible();
    await navigation.getByRole("button", { name: "Close" }).click();
  }

  await dialog.getByRole("button", { name: "Search in PDF" }).click();
  const search = dialog.getByRole("textbox", { name: "Find in PDF" });
  await search.fill("Second Page");
  await expect
    .poll(async () => dialog.locator("[data-pdf-findbar]").innerText())
    .toMatch(/1\s*\/\s*1/);

  await page.keyboard.press("Escape");
  await expect(dialog.locator("[data-pdf-findbar]")).toBeHidden();

  if (viewportWidth >= 1280) {
    await dialog.getByRole("button", { name: "Highlight" }).click();
  } else {
    await dialog.getByRole("button", { name: "PDF reader tools" }).click();
    await page.getByRole("button", { name: "Highlight" }).click();
  }
  await expect(dialog.locator(".textLayer.highlighting").first()).toBeVisible();

  const editedDownload = page.waitForEvent("download");
  if (viewportWidth >= 1280) {
    await dialog.getByRole("button", { name: "Save edited PDF copy" }).click();
  } else {
    await page.getByRole("button", { name: "Save copy" }).click();
  }
  await expect
    .poll(async () => (await editedDownload).suggestedFilename())
    .toBe("reader-sample-edited.pdf");

  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  expect(errors).toEqual([]);
});

test("the thumbnail panel of a long document grows as it is scrolled", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await installViewerApi(page);
  await page.goto("/files?view=list");
  await openFile(page, "reader-long.pdf");

  const dialog = page.getByRole("dialog", { name: "reader-long.pdf" });
  await expect(dialog).toBeVisible();

  // The sidebar is an aside on a wide viewport and a drawer on a narrow one, so the
  // tiles are read from whichever arrangement the viewport uses.
  const viewportWidth = page.viewportSize()?.width ?? 0;
  if (viewportWidth < 1024) {
    await dialog.getByRole("button", { name: "Open PDF sidebar" }).click();
  }
  const host =
    viewportWidth >= 1024
      ? dialog
      : page.getByRole("dialog", { name: "Document navigation" });
  const tiles = host.getByRole("button", { name: /^Go to page / });
  await expect(tiles.first()).toBeVisible({ timeout: 30_000 });
  await expect(host.getByRole("button", { name: "Go to page 1", exact: true })).toBeVisible();

  // The panel renders a batch of tiles rather than one per page, so the last page of
  // a hundred and twenty page document is not mounted while the first is on screen.
  const mounted = await tiles.count();
  expect(mounted).toBeLessThan(120);
  await expect(host.getByRole("button", { name: "Go to page 120", exact: true })).toHaveCount(0);

  const panel = host.locator('[role="tabpanel"]').first();
  await panel.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
    element.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await expect
    .poll(() => tiles.count(), { timeout: 15_000 })
    .toBeGreaterThan(mounted);
  expect(errors).toEqual([]);
});

test("mobile EPUB navigation opens in a HeroUI drawer", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installViewerApi(page);
  await page.goto("/files?view=list");
  await openFile(page, "reader-sample.epub");

  const dialog = page.getByRole("dialog", { name: "reader-sample.epub" });
  await expect(dialog.locator("[data-epub-reader]")).toBeVisible();
  const foliate = dialog.locator("foliate-view");
  await expect(foliate).toHaveAttribute("data-rendered-content", /A Quiet Beginning/);

  const menu = dialog.getByRole("button", { name: "Open ebook navigation" });
  await expect(menu).toBeVisible();
  await menu.click();

  const drawer = page.getByRole("dialog", { name: "Book navigation" });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole("listbox", { name: "Table of contents" })).toBeVisible();
  await drawer.getByRole("option", { name: "Across the Cloud" }).click();
  await expect(drawer).toBeHidden();
  await expect(dialog.getByText("Across the Cloud").first()).toBeVisible();
});

test("EPUB renders in its dedicated reader, navigates, and closes cleanly", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.stack || error.message));
  await installViewerApi(page);
  await page.goto("/files?view=list");
  await openFile(page, "reader-sample.epub");

  const dialog = page.getByRole("dialog", { name: "reader-sample.epub" });
  await expect(dialog).toBeVisible();
  await expect(dialog.locator("[data-epub-reader]")).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Open ebook navigation" })).toBeVisible();
  await expect(dialog.getByText("TelDrive Reader Fixture").first()).toBeVisible();

  const foliate = dialog.locator("foliate-view");
  await expect
    .poll(() =>
      foliate.evaluate((element) => {
        const view = element as FoliateView;
        return `${String(view.book?.metadata?.title || "")}:${view.book?.sections?.length ?? 0}`;
      }),
    )
    .toBe("TelDrive Reader Fixture:2");
  await expect(foliate).toHaveAttribute("data-rendered-content", /A Quiet Beginning/);
  await expect
    .poll(() =>
      foliate.evaluate((element) => {
        const view = element as FoliateView;
        return {
          flow: view.renderer?.getAttribute("flow"),
          gap: view.renderer?.getAttribute("gap"),
          columns: view.renderer?.getAttribute("max-column-count"),
        };
      }),
    )
    .toEqual({ flow: "paginated", gap: "6%", columns: "2" });

  await expect(foliate).toBeVisible();
  const headerBox = await dialog.locator("[data-epub-header]").boundingBox();
  const canvasBox = await dialog.locator("[data-epub-canvas]").boundingBox();
  const footerBox = await dialog.locator("[data-epub-footer]").boundingBox();
  expect(headerBox && canvasBox && footerBox).toBeTruthy();
  expect(canvasBox!.y).toBeGreaterThanOrEqual(headerBox!.y + headerBox!.height);
  expect(canvasBox!.y + canvasBox!.height).toBeLessThanOrEqual(footerBox!.y);

  const viewportWidth = page.viewportSize()?.width ?? 0;
  if (viewportWidth >= 1024) {
    const canvasBefore = await dialog.locator("[data-epub-canvas]").boundingBox();
    await dialog.getByRole("button", { name: "Open ebook navigation" }).click();
    const sidebar = dialog.locator("[data-epub-sidebar]");
    await expect(sidebar).toBeVisible();
    await expect(sidebar.getByRole("listbox", { name: "Table of contents" })).toBeVisible();
    const canvasAfter = await dialog.locator("[data-epub-canvas]").boundingBox();
    expect(canvasBefore && canvasAfter).toBeTruthy();
    expect(canvasAfter!.x).toBeGreaterThan(canvasBefore!.x);
    await sidebar.getByRole("option", { name: "Across the Cloud" }).click();
  }
  await settleBrowserLayout(page);
  expect(errors).toEqual([]);

  await dialog.getByRole("button", { name: "Reading settings" }).click();
  await page.getByRole("button", { name: "Night" }).click();
  await expect(dialog.locator("[data-epub-reader]")).toHaveAttribute("data-reader-theme", "night");
  const appearance = page.getByRole("dialog", { name: "Reading appearance" });
  await appearance.getByRole("button", { name: "Done" }).click();
  await expect(appearance).toBeHidden();
  await settleBrowserLayout(page);
  expect(errors).toEqual([]);

  // Turning the page has to move the reader. Reader state is no longer persisted,
  // so the observable effect is the position label the footer shows. A wide
  // viewport followed the table of contents to the last section, where only the
  // previous page moves, and a narrow one is still at the start of the book.
  const position = dialog.locator("[data-epub-footer] p");
  const positionBefore = await position.innerText();
  const navigation = viewportWidth >= 1024 ? "Previous page" : "Next page";
  await dialog.getByRole("button", { name: navigation }).click();
  await expect.poll(() => position.innerText()).not.toBe(positionBefore);
  await settleBrowserLayout(page);
  expect(errors).toEqual([]);
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  expect(errors).toEqual([]);
});
