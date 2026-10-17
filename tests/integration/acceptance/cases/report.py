"""Usage report: query filters, pagination and trend/summary consistency."""

from __future__ import annotations

import re
from pathlib import Path

from ..report import check, check_eq, check_no_ledger_fields

CASES = []

UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
COST = 14


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _summary(ctx, **query):
    return ctx.usage_rows(**query)["summary"]


@case("report.filters_trend_page_consistent",
      "key/model/result/search filters, pagination and trend sums agree with the summary, including unpriced executions")
def filters_trend_page(ctx):
    root = Path(ctx.artifacts) / "host-report"
    ctx.recreate_host("report", root=root)
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="report-alpha")
    for _ in range(3):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "request accepted")
    ctx.wait_report(item["id"], settled=3)

    # A second key adds an unpriced execution: it must widen neither the priced
    # subtotal nor the priced execution count of the first key.
    unpriced_item, unpriced_key = ctx.new_key(label="report-unpriced")
    check_eq(ctx.client.chat(unpriced_key, "synthetic-unpriced").status, 200,
             "an unpriced request is accepted")
    ctx.wait_report(unpriced_item["id"], settled=1)

    report = ctx.usage_rows(key_id=item["id"])
    check_eq(report["total"], 3, "three matching requests")
    check_eq(report["summary"]["requests"], 3, "summary requests")
    check_eq(report["summary"]["cost_micro_usd"], 3 * COST, "summary cost")
    check_eq(report["summary"]["tokens"]["total"], 3 * COST, "summary tokens")
    check_eq(report["summary"]["complete_executions"], 3, "three complete executions")
    check_eq(report["summary"]["unpriced_executions"], 0, "the priced key has no unpriced execution")
    check_no_ledger_fields(report["summary"], "key summary")

    unpriced = ctx.usage_rows(key_id=unpriced_item["id"])
    check_eq(unpriced["summary"]["cost_micro_usd"], 0, "an unpriced key has no priced subtotal")
    check_eq(unpriced["summary"]["unpriced_executions"], 1, "the unpriced execution is explicit")

    # Filter agreement. An unfiltered report sees all four rows and one unpriced.
    everything = _summary(ctx)
    check_eq(everything["requests"], 4, "unfiltered summary sees every request")
    check_eq(everything["cost_micro_usd"], 3 * COST, "unfiltered cost is the priced subtotal only")
    check_eq(everything["unpriced_executions"], 1, "unfiltered summary counts the unpriced execution")

    by_model = ctx.usage_rows(model="synthetic-chat")
    check_eq(by_model["total"], 3, "model filter matches the priced model only")
    check_eq(by_model["summary"]["cost_micro_usd"], 3 * COST, "model filter cost")
    unpriced_model = ctx.usage_rows(model="synthetic-unpriced")
    check_eq(unpriced_model["total"], 1, "model filter matches the unpriced model")
    check_eq(unpriced_model["summary"]["unpriced_executions"], 1, "unpriced model filter count")

    succeeded = ctx.usage_rows(key_id=item["id"], result="succeeded")
    check_eq(succeeded["total"], 3, "result=succeeded matches settled successes")
    check(all(row["state"] == "settled" and row["outcome"] == "succeeded" for row in succeeded["items"]),
          "result=succeeded only returns settled successes")
    by_search = ctx.usage_rows(search="report-alpha")
    check_eq(by_search["total"], 3, "search matches the key label")

    # Pagination consistency.
    page1 = ctx.usage_rows(key_id=item["id"], page=1, page_size=2)
    page2 = ctx.usage_rows(key_id=item["id"], page=2, page_size=2)
    check_eq(len(page1["items"]), 2, "page 1 size")
    check_eq(len(page2["items"]), 1, "page 2 size")
    check_eq(page1["total"], 3, "total is all-match, not page-local")
    check_eq(page2["total"], 3, "total stable across pages")
    check_eq(page1["summary"]["cost_micro_usd"], 3 * COST,
             "page 1 summary covers the whole selection, not just the page")
    check_eq(page2["summary"]["cost_micro_usd"], 3 * COST,
             "page 2 summary covers the whole selection, not just the page")
    ids1 = {row["id"] for row in page1["items"]}
    ids2 = {row["id"] for row in page2["items"]}
    check(not (ids1 & ids2), "pages do not overlap")

    # Trend sums must equal the summary.
    trend = report["trend"]
    check(len(trend) >= 1, "trend has at least one bucket")
    check_eq(sum(point["cost_micro_usd"] for point in trend), report["summary"]["cost_micro_usd"],
             "trend cost sums to summary cost")
    check_eq(sum(point["unpriced_executions"] for point in trend),
             report["summary"]["unpriced_executions"],
             "trend unpriced count sums to the summary")
    check_eq(sum(point["tokens"]["total"] for point in trend), report["summary"]["tokens"]["total"],
             "trend tokens sum to summary tokens")
    check_eq(sum(point["requests"] for point in trend), report["summary"]["requests"],
             "trend requests sum to summary requests")

    # bucket=hour and bucket=day both stay consistent with the summary.
    for bucket in ("hour", "day"):
        bucketed = ctx.usage_rows(key_id=item["id"], bucket=bucket)
        check_eq(sum(point["cost_micro_usd"] for point in bucketed["trend"]), 3 * COST,
                 f"bucket={bucket} cost sum")
    return {"summary": report["summary"], "trend_points": len(trend),
            "page1": len(page1["items"]), "page2": len(page2["items"]),
            "unpriced": everything["unpriced_executions"]}


@case("report.trace_id_full_uuid",
      "request trace ids are full UUIDs and are searchable")
def trace_id(ctx):
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="report-trace")
    check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "request accepted")
    request = ctx.wait_until(lambda: ctx.find_request(key_id=item["id"]))
    check(request is not None, "request recorded")
    trace = request["trace_id"]
    check(UUID_RE.match(trace or ""), f"trace id is a full UUID: {trace!r}")
    check(request["id"] != trace, "lifecycle id and trace id are distinct")
    found = ctx.usage_rows(search=trace)
    check_eq(found["total"], 1, "search by trace id finds the request")
    check_eq(found["items"][0]["id"], request["id"], "trace search returns the right request")
    return {"trace_id": trace, "request_id": request["id"]}
