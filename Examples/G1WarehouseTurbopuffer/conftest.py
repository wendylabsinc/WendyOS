"""Fetch the pinned third-party assets (G1 model, policies and three.js) once before the tests run."""

from warehouse import assets


def pytest_sessionstart(session):
    assets.ensure()
