"""Fetch the pinned third-party assets (model and three.js) once before the tests run."""

from drone_formation import assets


def pytest_sessionstart(session):
    assets.ensure()
