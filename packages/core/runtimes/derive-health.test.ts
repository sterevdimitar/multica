import { describe, expect, it } from "vitest";
import type { AgentRuntime } from "../types";
import { deriveRuntimeHealth } from "./derive-health";

const FIXED_NOW = new Date("2026-04-27T12:00:00Z").getTime();

function makeRuntime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: "daemon-1",
    name: "Test Runtime",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: null,
    visibility: "private",
    last_seen_at: new Date(FIXED_NOW - 10_000).toISOString(),
    created_at: "2026-04-01T00:00:00Z",
    updated_at: "2026-04-01T00:00:00Z",
    ...overrides,
  };
}

describe("deriveRuntimeHealth", () => {
  it("returns online when status is online (regardless of last_seen_at)", () => {
    expect(
      deriveRuntimeHealth(makeRuntime({ status: "online", last_seen_at: null }), FIXED_NOW),
    ).toBe("online");
  });

  it("returns recently_lost when offline less than 5 minutes", () => {
    expect(
      deriveRuntimeHealth(
        makeRuntime({
          status: "offline",
          last_seen_at: new Date(FIXED_NOW - 2 * 60_000).toISOString(),
        }),
        FIXED_NOW,
      ),
    ).toBe("recently_lost");
  });

  it("returns offline when offline between 5 minutes and 6 days", () => {
    expect(
      deriveRuntimeHealth(
        makeRuntime({
          status: "offline",
          last_seen_at: new Date(FIXED_NOW - 60 * 60_000).toISOString(), // 1 hour
        }),
        FIXED_NOW,
      ),
    ).toBe("offline");
  });

  it("returns about_to_gc when offline beyond 6 days (within 1 day of GC)", () => {
    expect(
      deriveRuntimeHealth(
        makeRuntime({
          status: "offline",
          last_seen_at: new Date(FIXED_NOW - 6.5 * 24 * 3600_000).toISOString(),
        }),
        FIXED_NOW,
      ),
    ).toBe("about_to_gc");
  });

  it("treats null last_seen_at as long-offline (about_to_gc)", () => {
    // last_seen_at = null means lastSeen = 0 (epoch), so offlineFor is huge.
    expect(
      deriveRuntimeHealth(
        makeRuntime({ status: "offline", last_seen_at: null }),
        FIXED_NOW,
      ),
    ).toBe("about_to_gc");
  });

  it("respects the 5-minute boundary (just inside → recently_lost)", () => {
    expect(
      deriveRuntimeHealth(
        makeRuntime({
          status: "offline",
          last_seen_at: new Date(FIXED_NOW - (5 * 60_000 - 1_000)).toISOString(),
        }),
        FIXED_NOW,
      ),
    ).toBe("recently_lost");
  });

  it("respects the 5-minute boundary (just outside → offline)", () => {
    expect(
      deriveRuntimeHealth(
        makeRuntime({
          status: "offline",
          last_seen_at: new Date(FIXED_NOW - (5 * 60_000 + 1_000)).toISOString(),
        }),
        FIXED_NOW,
      ),
    ).toBe("offline");
  });
});

// Webhook runtimes (runtime placement, 2026-09-20): no heartbeat, so the
// health is the placement's — cap 0, the cool-down, else online. The
// placement columns are ignored on a daemon row.
describe("deriveRuntimeHealth — webhook runtimes", () => {
  const now = Date.parse("2026-09-20T12:00:00Z");
  const webhook = (extra: Partial<AgentRuntime>): AgentRuntime =>
    makeRuntime({ runtime_mode: "webhook", status: "online", ...extra });

  it("is online with no cap and no cool-down", () => {
    expect(deriveRuntimeHealth(webhook({}), now)).toBe("online");
    expect(deriveRuntimeHealth(webhook({ max_concurrent_tasks: null }), now)).toBe("online");
    expect(deriveRuntimeHealth(webhook({ max_concurrent_tasks: 3 }), now)).toBe("online");
  });

  it("is out of the rotation at cap 0", () => {
    expect(deriveRuntimeHealth(webhook({ max_concurrent_tasks: 0 }), now)).toBe("out_of_rotation");
  });

  it("is down while down_until is in the future, online once it has passed", () => {
    expect(deriveRuntimeHealth(webhook({ down_until: "2026-09-20T12:01:00Z" }), now)).toBe("down");
    expect(deriveRuntimeHealth(webhook({ down_until: "2026-09-20T11:59:00Z" }), now)).toBe("online");
    expect(deriveRuntimeHealth(webhook({ down_until: null }), now)).toBe("online");
  });

  it("cap 0 wins over a cool-down", () => {
    expect(
      deriveRuntimeHealth(webhook({ max_concurrent_tasks: 0, down_until: "2026-09-20T12:01:00Z" }), now),
    ).toBe("out_of_rotation");
  });

  it("ignores the placement columns on a daemon row", () => {
    const daemon = makeRuntime({ status: "online", down_until: "2026-09-20T12:01:00Z", max_concurrent_tasks: 0 });
    expect(deriveRuntimeHealth(daemon, now)).toBe("online");
  });
});
