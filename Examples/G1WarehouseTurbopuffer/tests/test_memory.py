import os
import uuid

import pytest

from warehouse import memory
from warehouse.catalog import INBOUND, QUESTION, SHELVED


def test_keyword_stand_in_routes_boxes_and_answers_questions():
    stand_in = memory.KeywordMemory()
    stand_in.reset()
    answer = stand_in.where_does("AA batteries, 24 pack")
    assert answer.purpose == "where"
    assert answer.best.zone == "power"
    stand_in.remember(INBOUND[0], "power", 1)
    found = stand_in.find("where are the AA batteries")
    assert found.purpose == "find"
    assert (found.best.label, found.best.zone, found.best.bay) == (INBOUND[0].label, "power", 1)
    assert all(match.kind == "item" for match in found.matches)


def test_memory_calls_run_off_the_caller_thread_and_are_logged(monkeypatch):
    monkeypatch.delenv("TURBOPUFFER_API_KEY", raising=False)
    store = memory.Memory()
    try:
        assert store.backend == "Keyword stand-in"
        store.submit("reset").result(timeout=5)
        answer = store.submit("where_does", "M3 hex keys").result(timeout=5)
        assert store.last is answer
        assert store.log[0] is answer
        assert store.calls == 2
    finally:
        store.close()


@pytest.mark.skipif(not os.environ.get("TURBOPUFFER_API_KEY"), reason="needs TURBOPUFFER_API_KEY")
def test_turbopuffer_shelves_the_charger_with_power_and_answers_the_request(monkeypatch):
    monkeypatch.setattr(memory, "NAMESPACE", f"wendy-g1-warehouse-test-{uuid.uuid4().hex[:8]}")
    store = memory.TurbopufferMemory(os.environ["TURBOPUFFER_API_KEY"])
    try:
        store.reset()
        answer = store.where_does(INBOUND[3].label)
        assert answer.best.zone == "power"
        assert answer.server_ms is not None
        store.remember(INBOUND[3], "power", 2)
        found = store.find(QUESTION)
        assert found.best.label == SHELVED["cables"].label   # stocked before: the DisplayPort cable
    finally:
        store.ns.delete_all()
