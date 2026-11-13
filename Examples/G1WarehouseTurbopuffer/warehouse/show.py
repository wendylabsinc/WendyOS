"""What the robot does, in order: shelve each inbound box where turbopuffer says similar
things live, then answer a question by fetching the best match back to the cart."""

from __future__ import annotations

from concurrent.futures import Future
from typing import TYPE_CHECKING, Iterator

from .catalog import BAYS, INBOUND, QUESTION, ZONES, zone
from .memory import zone_name
from .world import cart_slot, rack_slot

if TYPE_CHECKING:
    from .simulation import Simulation

LOOK = 1.5  # seconds to hold still while an answer is on screen


def _await(sim: "Simulation", future: Future) -> Iterator[None]:
    """Keep the robot balancing (and the world stepping) while a memory call is in flight."""
    while not future.done():
        yield from sim.skills.wait(0.02)
    return future.result()


def _ask(sim: "Simulation", name: str, *args, attempts: int = 3) -> Iterator[None]:
    """A memory call, retried after a pause if it fails (a network blip should not end the show)."""
    for attempt in range(attempts):
        try:
            return (yield from _await(sim, sim.memory.submit(name, *args)))
        except Exception as error:
            if attempt == attempts - 1:
                raise
            title, detail = sim.activity
            sim.say("Retrying turbopuffer", f"{type(error).__name__}: {error}"[:120])
            yield from sim.skills.wait(1.0 + attempt)
            sim.say(title, detail)


def run(sim: "Simulation") -> Iterator[None]:
    skills = sim.skills
    while True:
        sim.restock()
        sim.target = None
        sim.say("Loading memory", f"{sim.memory.backend}: zones and shelved items")
        while True:
            try:
                yield from _ask(sim, "reset")
                break
            except Exception as error:   # unreachable for now: wait, then try again
                sim.say("Waiting for turbopuffer", f"{type(error).__name__}: {error}"[:120])
                yield from skills.wait(5.0)
        occupied = {z.key: {0} for z in ZONES}

        for index, item in enumerate(INBOUND):
            name = f"box_{item.key}"
            sim.target = {"zone": "cart", "bay": index}
            sim.say("Picking up", item.label)
            yield from skills.pick(name, cart_slot(index))
            sim.target = None
            sim.say("Asking turbopuffer", f"Where do things like “{item.label}” go?")
            try:
                answer = yield from _ask(sim, "where_does", item.label)
                zone_key = answer.best.zone
            except Exception:
                zone_key = ZONES[index % len(ZONES)].key   # memory unavailable: any zone with room
            free = sorted(set(range(BAYS)) - occupied[zone_key]) or [BAYS - 1]
            bay = free[0]
            occupied[zone_key].add(bay)
            sim.target = {"zone": zone_key, "bay": bay}
            sim.say("Shelving", f"{zone_name(zone_key)}, bay {bay + 1}")
            yield from skills.wait(LOOK)
            yield from skills.place(rack_slot(zone(zone_key), bay))
            sim.moved(name, zone_key, bay)
            sim.target = None
            sim.say("Remembering", f"{item.label}: {zone_name(zone_key)}, bay {bay + 1}")
            try:
                yield from _ask(sim, "remember", item, zone_key, bay)
            except Exception:
                pass   # the viewer shows the memory error; the box is shelved either way

        sim.say("Question", QUESTION)
        yield from skills.wait(2.0)
        try:
            answer = yield from _ask(sim, "find", QUESTION)
            matches = answer.matches
        except Exception:
            matches = []   # no answer: nothing to fetch this time
        # the closest item the robot can carry (top-shelf stock is out of its reach)
        best = next((m for m in matches if sim.box_for(m.label)), None)
        name = sim.box_for(best.label) if best else None
        if name is not None:
            where = sim.location[name]
            sim.target = {"zone": where[0], "bay": where[1]}
            sim.say("Fetching", f"{best.label} from {zone_name(where[0])}, bay {where[1] + 1}")
            yield from skills.wait(LOOK)
            yield from skills.pick(name, rack_slot(zone(where[0]), where[1]))
            sim.target = {"zone": "cart", "bay": 0}
            sim.say("Bringing it to the cart", best.label)
            yield from skills.place(cart_slot(0))
            sim.moved(name, "cart", 0)
            sim.target = None
            sim.say("Here you go", best.label)
        yield from skills.wait(6.0)
        sim.say("Restocking", "The next shipment arrives")
        yield from skills.wait(2.0)
