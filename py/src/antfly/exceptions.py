"""Custom exception hierarchy for the Antfly SDK."""

from __future__ import annotations


class AntflyError(Exception):
    """Base exception for all Antfly SDK errors."""


class AntflyConnectionError(AntflyError):
    """Raised when the SDK cannot connect to the Antfly server."""


class AntflyAuthError(AntflyError):
    """Raised when authentication or authorization fails."""
