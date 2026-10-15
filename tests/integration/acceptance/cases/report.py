"""Usage report: query filters, pagination and trend/summary consistency."""

from __future__ import annotations

import re
from pathlib import Path

from ..report import check, check_eq

CASES = []

UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


def _spent(ctx, key_id):
    row = next((k for k in ctx.manager.keys().json()["items"] if k["id"] == key_id), None)
    return row["spent_micro_usd"] if row else None


@case("report.filters_trend_page_consistent",
      "key/model/result/search filters, pagination and trend sums agree with the summary")
def filters_trend_page(ctx):
    root = Path(ctx.artifacts) / "host-report"
    ctx.recreate_host("report", root=root)
    ctx.set_price("synthetic-chat")
    item, key = ctx.new_key(label="report-alpha")
    for _ in range(3):
        check_eq(ctx.client.chat(key, "synthetic-chat").status, 200, "request accepted")
    check(ctx.wait_until(lambda: _spent(ctx, item["id"]) == 42), "three requests settled at 42")

    report = ctx.usage_rows(key_id=item["id"])
    check_eq(report["total"], 3, "three matching requests")
    check_eq(report["summary"]["requests"], 3, "summary requests")
    check_eq(report["summary"]["cost_micro_usd"], 42, "summary cost")
    check_eq(report["summary"]["tokens"]["total"], 42, "summary tokens")

    # Filter agreement.
    by_model = ctx.usage_rows(model="synthetic-chat")
    check(by_model["total"] >= 3, "model filter matches")
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
    ids1 = {row["id"] for row in page1["items"]}
    ids2 = {row["id"] for row in page2["items"]}
    check(not (ids1 & ids2), "pages do not overlap")

    # Trend sums must equal the summary.
    trend = report["trend"]
    check(len(trend) >= 1, "trend has at least one bucket")
    check_eq(sum(point["cost_micro_usd"] for point in trend), report["summary"]["cost_micro_usd"],
             "trend cost sums to summary cost")
    check_eq(sum(point["tokens"]["total"] for point in trend), report["summary"]["tokens"]["total"],
             "trend tokens sum to summary tokens")
    check_eq(sum(point["requests"] for point in trend), report["summary"]["requests"],
             "trend requests sum to summary requests")

    # bucket=hour and bucket=day both stay consistent with the summary.
    for bucket in ("hour", "day"):
        bucketed = ctx.usage_rows(key_id=item["id"], bucket=bucket)
        check_eq(sum(point["cost_micro_usd"] for point in bucketed["trend"]), 42, f"bucket={bucket} cost sum")
    return {"summary": report["summary"], "trend_points": len(trend),
            "page1": len(page1["items"]), "page2": len(page2["items"])}


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
