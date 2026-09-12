"""Module docstring with non-ASCII: héllo → 日本."""
import os.path
from collections import OrderedDict as OD

GREETING = "héllo → 日本"


class Server:
    """A server."""

    port = 8080

    def start(self, name):
        """Start the server."""
        def inner():
            return helper(name)
        return inner()


def helper(name):
    return os.path.join(GREETING, name)


def test_helper():
    assert helper("x")
