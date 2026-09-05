import { describe, it, expect, vi } from "vitest";
import {
  searchUrl,
  framingFor,
  formatDistance,
  makeSearchRunner,
  MIN_QUERY_LENGTH,
  type SearchHit,
} from "./chartSearch";

function hit(over: Partial<SearchHit> = {}): SearchHit {
  return {
    name: "Brenton Reef Light",
    class: "LIGHTS",
    label: "Light",
    cell: "US5RI1BB",
    lat: 41.4,
    lng: -71.4,
    bbox: [-71.4, 41.4, -71.4, 41.4],
    distance_meters: 1852,
    ...over,
  };
}

describe("searchUrl", () => {
  it("sends the query alone by default", () => {
    const url = new URL(searchUrl("https://charts.example", "brenton"));
    expect(url.pathname).toBe("/noaa-enc/search");
    expect(url.searchParams.get("q")).toBe("brenton");
    expect(url.searchParams.has("lat")).toBe(false);
  });

  it("adds the origin so results come back nearest-first", () => {
    const url = new URL(
      searchUrl("", "north channel", { origin: { lat: 41.5, lng: -71.3 } }),
      "http://x"
    );
    expect(url.searchParams.get("lat")).toBe("41.5");
    expect(url.searchParams.get("lon")).toBe("-71.3");
  });

  it("passes limit and class through", () => {
    const url = new URL(searchUrl("", "reef", { limit: 5, objectClass: "LIGHTS" }), "http://x");
    expect(url.searchParams.get("limit")).toBe("5");
    expect(url.searchParams.get("class")).toBe("LIGHTS");
  });

  it("encodes queries with spaces and punctuation", () => {
    const url = new URL(searchUrl("", "point judith (n)"), "http://x");
    expect(url.searchParams.get("q")).toBe("point judith (n)");
  });
});

describe("framingFor", () => {
  it("centres on a point feature rather than fitting a zero extent", () => {
    expect(framingFor(hit())).toEqual({ kind: "center", zoom: 15 });
  });

  it("fits an area feature to its extent", () => {
    expect(framingFor(hit({ bbox: [-71.6, 41.3, -71.2, 41.6] }))).toEqual({ kind: "fit" });
  });
});

describe("formatDistance", () => {
  it("gives a decimal for close things and a whole number for far ones", () => {
    expect(formatDistance(1852)).toBe("1.0 nm");
    expect(formatDistance(1852 * 42)).toBe("42 nm");
  });

  it("is empty when the distance is unknown", () => {
    expect(formatDistance(-1)).toBe("");
  });
});

describe("makeSearchRunner", () => {
  it("does not query for a query shorter than the minimum", async () => {
    const run = vi.fn(async () => [hit()]);
    const runner = makeSearchRunner(run, [], 0);
    const onResult = vi.fn();
    const short = "brenton".slice(0, MIN_QUERY_LENGTH - 1);
    runner.search(short, onResult, vi.fn());
    expect(run).not.toHaveBeenCalled();
    expect(onResult).toHaveBeenCalledWith([], short);
  });

  it("debounces to a single query for a burst of keystrokes", async () => {
    vi.useFakeTimers();
    const run = vi.fn(async () => [hit()]);
    const runner = makeSearchRunner(run, [], 250);
    const onResult = vi.fn();
    for (const q of ["bre", "bren", "brent", "brenton"]) runner.search(q, onResult, vi.fn());
    await vi.advanceTimersByTimeAsync(300);
    expect(run).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenCalledWith("brenton");
    vi.useRealTimers();
  });

  it("drops a slow earlier response so it can't overwrite a newer one", async () => {
    vi.useFakeTimers();
    const slow = hit({ name: "SLOW" });
    const fast = hit({ name: "FAST" });
    let call = 0;
    const run = vi.fn(async (q: string) => {
      call++;
      // First query resolves after the second one has already been issued.
      if (call === 1) return new Promise<SearchHit[]>((r) => setTimeout(() => r([slow]), 500));
      return [fast];
    });
    const runner = makeSearchRunner(run, [], 0);
    const onResult = vi.fn();

    runner.search("first", onResult, vi.fn());
    await vi.advanceTimersByTimeAsync(1);
    runner.search("second", onResult, vi.fn());
    await vi.advanceTimersByTimeAsync(600);

    const names = onResult.mock.calls.map((c) => c[0][0]?.name);
    expect(names).toContain("FAST");
    expect(names).not.toContain("SLOW");
    vi.useRealTimers();
  });

  it("cancel stops a pending search from firing", async () => {
    vi.useFakeTimers();
    const run = vi.fn(async () => [hit()]);
    const runner = makeSearchRunner(run, [], 250);
    runner.search("brenton", vi.fn(), vi.fn());
    runner.cancel();
    await vi.advanceTimersByTimeAsync(300);
    expect(run).not.toHaveBeenCalled();
    vi.useRealTimers();
  });
});
