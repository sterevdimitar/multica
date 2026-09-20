// Derived "health" type for runtimes — the user-facing state we display
// in lists, cards, and tooltips. The raw server field is binary (online /
// offline + last_seen_at); this enum splits the offline state into three
// time-bucketed flavors so users can tell "just lost" from "long gone".

export type RuntimeHealth =
  | "online" // green — within heartbeat threshold
  | "recently_lost" // amber — offline < 5 minutes (likely transient)
  | "offline" // grey — offline 5 minutes ~ 7 days
  | "about_to_gc" // dim — within 1 day of the 7-day GC threshold
  // Webhook runtimes only (runtime placement, 2026-09-20): the probe, a
  // refused dispatch or a never-started run put the runtime in a cool-down
  // (down_until in the future) — amber, with the reason in the cell.
  | "down"
  // Webhook runtimes only: cap 0, the owner's manual switch. Grey.
  | "out_of_rotation";
