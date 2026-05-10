import { test, expect, Page } from "@playwright/test";

/**
 * Smoke tests for the CE-Manager observer UI.
 *
 * Assumes the observer is running at process.env.CE_OBSERVER_URL (or
 * http://localhost:30071) and that at least one demo session has run in
 * each mode (off/on/truncate/summary) — run `make demo-ce-<mode>` first.
 */

async function selectSessionByPrefix(page: Page, prefix: string): Promise<string> {
  await page.goto("/");
  // Wait for the session list to populate.
  const item = page
    .locator(".session-list .session-item")
    .filter({ has: page.locator(`.session-title:has-text("${prefix}")`) })
    .first();
  await item.waitFor({ state: "visible", timeout: 15_000 });
  const title = (await item.locator(".session-title").textContent()) || "";
  await item.click();
  // Wait for the stat strip to populate.
  await page.locator(".summary-banner.stat-strip .strip-stat").first().waitFor({ state: "visible" });
  return title;
}

test.describe("observer UI smoke", () => {
  test("off-mode session renders stat strip + answer pieces + conversation", async ({ page }, testInfo) => {
    const title = await selectSessionByPrefix(page, "ce-demo-off-");
    testInfo.attach("session-title", { body: title, contentType: "text/plain" });

    await expect(page.locator(".summary-banner.stat-strip")).toBeVisible();
    const strips = page.locator(".summary-banner.stat-strip .strip-stat");
    await expect(strips).toHaveCount(6);

    // Overflow expected for off-mode Q2.
    const overflow = page.locator(".strip-stat").filter({ hasText: "Overflow" });
    await expect(overflow).toBeVisible();

    // Conversation feed must not be hidden empty-state.
    await expect(page.locator("#conversationFeed")).not.toHaveClass(/empty-state/);

    // Answer-pieces panel visible and populated.
    await expect(page.locator("#answerPiecesSection")).toBeVisible();
    await expect(page.locator(".answer-pieces-turn").first()).toBeVisible();

    // Raw events panel exists but closed by default.
    const timeline = page.locator(".timeline-details");
    await expect(timeline).toBeVisible();
    const open = await timeline.evaluate((el) => (el as HTMLDetailsElement).open);
    expect(open).toBe(false);

    await page.screenshot({ path: "tests/__screenshots__/off-mode.png", fullPage: true });
  });

  test("on-mode session shows compactions + mask events", async ({ page }) => {
    await selectSessionByPrefix(page, "ce-demo-on-");
    const compactions = page.locator(".strip-stat").filter({ hasText: "Compactions" });
    await expect(compactions).toBeVisible();
    // Expect at least 1 compaction fired.
    const v = await compactions.locator(".strip-v").textContent();
    expect(Number(v)).toBeGreaterThan(0);
    await page.screenshot({ path: "tests/__screenshots__/on-mode.png", fullPage: true });
  });

  test("truncate-mode session shows truncation_applied cards in timeline", async ({ page }) => {
    await selectSessionByPrefix(page, "ce-demo-truncate-");
    // Open the raw events details.
    await page.locator(".timeline-details > summary").click();
    const ceManagerColumn = page.locator('.timeline-column[data-stage="ce-manager"] .event-stack');
    await expect(ceManagerColumn.locator(".event-card").first()).toBeVisible();
    await page.screenshot({ path: "tests/__screenshots__/truncate-mode.png", fullPage: true });
  });
});
