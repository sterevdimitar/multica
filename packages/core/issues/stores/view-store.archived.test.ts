import { describe, expect, it } from "vitest";
import { ALL_STATUSES, DEFAULT_VISIBLE_STATUSES } from "../config";
import { createIssueViewStore } from "./view-store";

// The default view (no status filter active) is DEFAULT_VISIBLE_STATUSES —
// every status but `archived`. hideStatus/showStatus must start from that
// list when no filter is active, otherwise hiding one column would re-show
// the archive, and showing the archive would be a no-op.
describe("view-store: archived is hidden by default", () => {
  it("DEFAULT_VISIBLE_STATUSES is ALL_STATUSES without archived", () => {
    expect(ALL_STATUSES).toContain("archived");
    expect(DEFAULT_VISIBLE_STATUSES).not.toContain("archived");
    expect(DEFAULT_VISIBLE_STATUSES).toEqual(ALL_STATUSES.filter((s) => s !== "archived"));
  });

  it("hideStatus with no filter active hides that status and keeps archived hidden", () => {
    const store = createIssueViewStore("test_view_store_archived_hide");
    store.getState().hideStatus("done");
    const filters = store.getState().statusFilters;
    expect(filters).toEqual(DEFAULT_VISIBLE_STATUSES.filter((s) => s !== "done"));
    expect(filters).not.toContain("archived");
  });

  it("showStatus('archived') with no filter active pins the default view plus archived", () => {
    const store = createIssueViewStore("test_view_store_archived_show");
    store.getState().showStatus("archived");
    expect(store.getState().statusFilters).toEqual([...DEFAULT_VISIBLE_STATUSES, "archived"]);
  });

  it("showStatus of an already-visible status with no filter active is a no-op", () => {
    const store = createIssueViewStore("test_view_store_archived_show_noop");
    store.getState().showStatus("done");
    expect(store.getState().statusFilters).toEqual([]);
  });

  it("clearFilters returns to the default view (archived hidden again)", () => {
    const store = createIssueViewStore("test_view_store_archived_clear");
    store.getState().showStatus("archived");
    store.getState().clearFilters();
    expect(store.getState().statusFilters).toEqual([]);
  });
});
