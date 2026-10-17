"""Module docstring."""
from __future__ import annotations

import os, sys
from typing import TYPE_CHECKING, Optional

MAX_RETRIES: int = 3
default_name = "demo"

type Alias = list[int]

# Describes the service.
@dataclass(frozen=True)
class Service(Base, metaclass=Meta):
    """Service docstring."""

    TIMEOUT = 30
    name: str = "svc"

    class Inner:
        def ping(self):
            return 1

    def __init__(self, name: str) -> None:
        self.name = name

    @staticmethod
    def make() -> "Service":
        return Service("x")

    async def fetch(self, url: str) -> bytes:  # trailing comment
        def helper():
            return url
        return helper()


def helper(a, b=2, *args, **kwargs):
    return a + b


async def main() -> None:
    await Service.make().fetch("u")


if __name__ == "__main__":
    main()
