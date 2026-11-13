"""The robot's shelf memory, stored in turbopuffer.

Every stored item and every zone description is a row whose `label` text
turbopuffer embeds itself (native embeddings), so the robot sends plain text and
asks plain questions. Two questions matter:

- where_does(label): which stored item or zone is most like this new box?
- find(question): which stored item answers this request?

Without TURBOPUFFER_API_KEY the app falls back to a small keyword stand-in so it
still runs; the viewer says so.
"""

from __future__ import annotations

import os
import re
import threading
import time
from collections import deque
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field

from .catalog import SHELVED, STOCK, STOCK_BAYS, ZONES, Item, zone

NAMESPACE = os.environ.get("TURBOPUFFER_NAMESPACE", "wendy-g1-warehouse")
REGION = os.environ.get("TURBOPUFFER_REGION", "gcp-us-central1")
# Not every model runs in every region: check turbopuffer's model list if you change either.
EMBED_MODEL = os.environ.get("TURBOPUFFER_EMBED_MODEL", "nvidia/nemotron-3-embed-8b")
ATTRIBUTES = ["label", "kind", "zone", "shelf", "bay"]


@dataclass
class Match:
    label: str
    kind: str       # "item" or "zone" (a zone's description)
    zone: str
    shelf: str      # "work" (bays the robot uses), "top" (fixed stock), or "" for a zone
    bay: int
    distance: float


@dataclass
class Answer:
    operation: str            # "query" or "write"
    text: str                 # the label or question sent
    matches: list[Match] = field(default_factory=list)
    round_trip_ms: float = 0.0
    server_ms: float | None = None
    rows: int | None = None   # approximate namespace size
    note: str = ""            # for writes: where the item went
    purpose: str = ""         # for queries: "where" (a new box) or "find" (a question)

    @property
    def best(self) -> Match | None:
        return self.matches[0] if self.matches else None


def _row(item: Item, zone_key: str, shelf: str, bay: int) -> dict:
    return {"id": f"item-{item.key}", "label": item.label, "kind": "item", "zone": zone_key, "shelf": shelf, "bay": bay}


def _seed_rows() -> list[dict]:
    """What the memory knows before the first box: each zone's description and what is on its shelves."""
    rows = [{"id": f"zone-{z.key}", "label": z.description, "kind": "zone", "zone": z.key, "shelf": "", "bay": -1}
            for z in ZONES]
    rows += [_row(item, key, "work", 0) for key, item in SHELVED.items()]
    rows += [_row(item, key, "top", bay) for key, items in STOCK.items() for bay, item in zip(STOCK_BAYS, items)]
    return rows


class TurbopufferMemory:
    backend = "turbopuffer"

    def __init__(self, api_key: str):
        import turbopuffer  # imported here so the stand-in works without the package

        self.client = turbopuffer.Turbopuffer(api_key=api_key, region=REGION)
        self.ns = self.client.namespace(NAMESPACE)
        self.detail = f"{REGION} · {NAMESPACE} · {EMBED_MODEL}"

    def describe(self) -> list[str]:
        return ["Memory: turbopuffer", f"Namespace: {NAMESPACE} ({REGION})", f"Embedding model: {EMBED_MODEL}"]

    def reset(self) -> Answer:
        started = time.perf_counter()
        try:
            self.ns.delete_all()
        except Exception:  # a fresh namespace does not exist yet
            pass
        rows = _seed_rows()
        self._write(rows)
        return Answer("write", "Zones and shelved items", round_trip_ms=(time.perf_counter() - started) * 1000,
                      rows=len(rows), note=f"{len(rows)} rows")

    def _write(self, rows: list[dict]):
        return self.ns.write(
            upsert_rows=rows,
            distance_metric="cosine_distance",
            schema={"label": {"type": "string", "embed": {"model": EMBED_MODEL}},
                    "kind": {"type": "string", "filterable": True},
                    "zone": {"type": "string", "filterable": True},
                    "shelf": {"type": "string", "filterable": True},
                    "bay": {"type": "int", "filterable": True}},
        )

    def _query(self, text: str, filters=None, limit: int = 3) -> Answer:
        started = time.perf_counter()
        kwargs = dict(rank_by=("label", "ANN", ("Embed", text)), limit=limit, include_attributes=ATTRIBUTES)
        if filters is not None:
            kwargs["filters"] = filters
        response = self.ns.query(**kwargs)
        answer = Answer("query", text, round_trip_ms=(time.perf_counter() - started) * 1000)
        for row in response.rows or []:
            answer.matches.append(Match(str(row["label"]), str(row["kind"]), str(row["zone"]), str(row["shelf"]), int(row["bay"]),
                                        float(row["$dist"])))
        performance = response.performance
        if performance is not None:
            answer.server_ms = performance.server_total_ms
            answer.rows = performance.approx_namespace_size
        return answer

    def where_does(self, label: str) -> Answer:
        answer = self._query(label)
        answer.purpose = "where"
        return answer

    def find(self, question: str) -> Answer:
        answer = self._query(question, filters=("kind", "Eq", "item"))
        answer.purpose = "find"
        return answer

    def remember(self, item: Item, zone_key: str, bay: int) -> Answer:
        started = time.perf_counter()
        self._write([_row(item, zone_key, "work", bay)])
        return Answer("write", item.label, round_trip_ms=(time.perf_counter() - started) * 1000,
                      note=f"{zone_name(zone_key)}, bay {bay + 1}")


class KeywordMemory:
    """Offline stand-in: word overlap instead of embeddings. Not turbopuffer."""

    backend = "Keyword stand-in"
    detail = "Set TURBOPUFFER_API_KEY to use turbopuffer"

    def describe(self) -> list[str]:
        return ["Memory: keyword stand-in (set TURBOPUFFER_API_KEY to use turbopuffer)"]
    SYNONYMS = {"charge": "charger", "charging": "charger", "power": "charger", "battery": "batteries",
                "screws": "screw", "screwdriver": "screwdrivers", "adapter": "adapters", "cable": "cables"}

    def __init__(self):
        self.rows: dict[str, dict] = {}

    @classmethod
    def _words(cls, text: str) -> set[str]:
        words = set(re.findall(r"[a-z0-9]+", text.lower()))
        return words | {cls.SYNONYMS[w] for w in words if w in cls.SYNONYMS}

    def reset(self) -> Answer:
        self.rows = {row["id"]: row for row in _seed_rows()}
        return Answer("write", "Zones and shelved items", rows=len(self.rows), note=f"{len(self.rows)} rows")

    def _query(self, text: str, kinds: tuple[str, ...]) -> Answer:
        started = time.perf_counter()
        query = self._words(text)
        scored = []
        for row in self.rows.values():
            if row["kind"] not in kinds:
                continue
            words = self._words(row["label"])
            overlap = len(query & words) / max(1, len(query | words))
            scored.append((1.0 - overlap, row))
        scored.sort(key=lambda pair: pair[0])
        answer = Answer("query", text, round_trip_ms=(time.perf_counter() - started) * 1000, rows=len(self.rows))
        answer.matches = [Match(r["label"], r["kind"], r["zone"], r["shelf"], r["bay"], round(d, 3)) for d, r in scored[:3]]
        return answer

    def where_does(self, label: str) -> Answer:
        answer = self._query(label, ("item", "zone"))
        answer.purpose = "where"
        return answer

    def find(self, question: str) -> Answer:
        answer = self._query(question, ("item",))
        answer.purpose = "find"
        return answer

    def remember(self, item: Item, zone_key: str, bay: int) -> Answer:
        self.rows[f"item-{item.key}"] = _row(item, zone_key, "work", bay)
        return Answer("write", item.label, rows=len(self.rows), note=f"{zone_name(zone_key)}, bay {bay + 1}")


class Memory:
    """Runs memory calls off the physics thread and keeps the last exchange for the viewer."""

    def __init__(self):
        key = os.environ.get("TURBOPUFFER_API_KEY", "").strip()
        self.impl = TurbopufferMemory(key) if key else KeywordMemory()
        self.pool = ThreadPoolExecutor(max_workers=1, thread_name_prefix="memory")
        self.last: Answer | None = None            # the latest query, for the viewer
        self.log: deque[Answer] = deque(maxlen=6)  # the latest queries and writes
        self.error: str | None = None
        self.calls = 0
        self._lock = threading.Lock()              # the worker thread writes; the web server reads

    @property
    def backend(self) -> str:
        return self.impl.backend

    @property
    def detail(self) -> str:
        return self.impl.detail

    def describe(self) -> list[str]:
        """Startup log lines: which memory is answering."""
        return self.impl.describe()

    def recent(self) -> tuple[Answer | None, list[Answer]]:
        """The latest query and the latest operations, safe to read from any thread."""
        with self._lock:
            return self.last, list(self.log)

    def submit(self, name: str, *args) -> Future:
        def call():
            try:
                answer = getattr(self.impl, name)(*args)
            except Exception as error:  # surfaced in the viewer; the robot falls back to its own guess
                self.error = f"{type(error).__name__}: {error}"[:300]
                raise
            with self._lock:
                self.calls += 1
                self.error = None
                if name == "reset":   # a new loop of the show: start the viewer's log afresh
                    self.last = None
                    self.log.clear()
                if answer.operation == "query":
                    self.last = answer
                self.log.appendleft(answer)
            return answer
        return self.pool.submit(call)

    def close(self) -> None:
        self.pool.shutdown(wait=False, cancel_futures=True)


def zone_name(key: str) -> str:
    return "Cart" if key == "cart" else zone(key).name
