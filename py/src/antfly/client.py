"""Main client interface for Antfly SDK."""

from __future__ import annotations

import base64
from typing import Any
from urllib.parse import quote

import httpx
from httpx import Timeout

from antfly.client_generated import Client
from antfly.client_generated.api.data_operations import (
    batch_write as batch,
)
from antfly.client_generated.api.data_operations import (
    lookup_key,
)
from antfly.client_generated.client import (
    AuthenticatedClient,
)
from antfly.client_generated.models import (
    BatchRequest,
    BatchRequestInserts,
    Error,
)
from antfly.client_generated.types import UNSET

from .exceptions import AntflyException


DEFAULT_TIMEOUT = 30.0


class AntflyClient:
    """High-level client for interacting with Antfly database."""

    __slots__ = (
        "base_url",
        "_client",
        "_httpx_client",
    )

    def __init__(
        self,
        base_url: str,
        *,
        username: str | None = None,
        password: str | None = None,
        api_key: tuple[str, str] | None = None,
        bearer_token: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        verify_ssl: bool = True,
        headers: dict[str, str] | None = None,
    ) -> None:
        """
        Initialize Antfly client.

        Authentication methods are mutually exclusive.

        Supported auth:
        - Basic auth
        - API key auth
        - Bearer token auth

        Args:
            base_url:
                Base URL of Antfly server.

            username:
                Username for basic authentication.

            password:
                Password for basic authentication.

            api_key:
                Tuple of (key_id, key_secret).

            bearer_token:
                Bearer token string.

            timeout:
                Request timeout in seconds.

            verify_ssl:
                Enable SSL certificate verification.

            headers:
                Additional headers.
        """
        self.base_url = base_url.rstrip("/")

        self._validate_auth(
            username=username,
            password=password,
            api_key=api_key,
            bearer_token=bearer_token,
        )

        httpx_args: dict[str, Any] = {
            "verify": verify_ssl,
            "headers": headers or {},
            "follow_redirects": True,
            "limits": httpx.Limits(
                max_connections=100,
                max_keepalive_connections=20,
                keepalive_expiry=30,
            ),
        }

        timeout_config = Timeout(
            connect=min(timeout, 5.0),
            read=timeout,
            write=timeout,
            pool=5.0,
        )

        if api_key is not None:
            self._client = self._build_api_key_client(
                api_key=api_key,
                timeout=timeout_config,
                httpx_args=httpx_args,
            )

        elif bearer_token is not None:
            self._client = self._build_bearer_client(
                token=bearer_token,
                timeout=timeout_config,
                httpx_args=httpx_args,
            )

        else:
            self._client = self._build_basic_client(
                username=username,
                password=password,
                timeout=timeout_config,
                httpx_args=httpx_args,
            )

        self._httpx_client = self._client.get_httpx_client()

    # --------------------------------------------------------------------- #
    # Internal helpers
    # --------------------------------------------------------------------- #

    @staticmethod
    def _validate_auth(
        *,
        username: str | None,
        password: str | None,
        api_key: tuple[str, str] | None,
        bearer_token: str | None,
    ) -> None:
        """Validate authentication configuration."""
        methods = sum(
            [
                api_key is not None,
                bearer_token is not None,
                username is not None or password is not None,
            ]
        )

        if methods > 1:
            raise ValueError(
                "Authentication methods are mutually exclusive"
            )

        if (username is None) != (password is None):
            raise ValueError(
                "Both username and password are required"
            )

    def _build_api_key_client(
        self,
        *,
        api_key: tuple[str, str],
        timeout: Timeout,
        httpx_args: dict[str, Any],
    ) -> AuthenticatedClient:
        """Create API key authenticated client."""
        key_id, key_secret = api_key

        encoded = base64.b64encode(
            f"{key_id}:{key_secret}".encode()
        ).decode()

        return AuthenticatedClient(
            base_url=self.base_url,
            token=encoded,
            prefix="ApiKey",
            timeout=timeout,
            httpx_args=httpx_args,
        )

    def _build_bearer_client(
        self,
        *,
        token: str,
        timeout: Timeout,
        httpx_args: dict[str, Any],
    ) -> AuthenticatedClient:
        """Create bearer token authenticated client."""
        return AuthenticatedClient(
            base_url=self.base_url,
            token=token,
            prefix="Bearer",
            timeout=timeout,
            httpx_args=httpx_args,
        )

    def _build_basic_client(
        self,
        *,
        username: str | None,
        password: str | None,
        timeout: Timeout,
        httpx_args: dict[str, Any],
    ) -> Client:
        """Create basic auth or anonymous client."""
        if username and password:
            httpx_args["auth"] = (username, password)

        return Client(
            base_url=self.base_url,
            timeout=timeout,
            httpx_args=httpx_args,
        )

    def _request(
        self,
        method: str,
        path: str,
        **kwargs: Any,
    ) -> Any:
        """
        Make an HTTP request.

        Raises:
            AntflyException:
                If request fails.
        """
        try:
            response = self._httpx_client.request(
                method,
                path,
                **kwargs,
            )

        except httpx.TimeoutException as exc:
            raise AntflyException(
                f"Request timed out: {method} {path}"
            ) from exc

        except httpx.NetworkError as exc:
            raise AntflyException(
                f"Network error during request: {method} {path}"
            ) from exc

        except httpx.HTTPError as exc:
            raise AntflyException(
                f"HTTP error during request: {method} {path}"
            ) from exc

        return self._handle_response(response)

    @staticmethod
    def _handle_response(response: httpx.Response) -> Any:
        """Handle HTTP response."""
        if response.status_code >= 400:
            raise AntflyException(
                AntflyClient._format_error(response)
            )

        if response.status_code == 204:
            return None

        content_type = response.headers.get(
            "content-type",
            "",
        )

        if "application/json" not in content_type:
            return response.text

        try:
            return response.json()

        except ValueError as exc:
            raise AntflyException(
                "Invalid JSON response from server"
            ) from exc

    @staticmethod
    def _format_error(response: httpx.Response) -> str:
        """Format API error response."""
        message = response.text

        try:
            data = response.json()

            if isinstance(data, dict):
                message = (
                    data.get("error")
                    or data.get("message")
                    or message
                )

        except Exception:
            pass

        return (
            f"Request failed "
            f"({response.status_code}): {message}"
        )

    @staticmethod
    def _quote(value: str) -> str:
        """URL encode path value."""
        return quote(value, safe="")

    # --------------------------------------------------------------------- #
    # Table operations
    # --------------------------------------------------------------------- #

    def create_table(
        self,
        name: str,
        *,
        num_shards: int | None = None,
        indexes: dict[str, Any] | None = None,
        schema: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """
        Create a new table.
        """
        body: dict[str, Any] = {}

        if num_shards is not None:
            body["num_shards"] = num_shards

        if indexes is not None:
            body["indexes"] = indexes

        if schema is not None:
            body["schema"] = schema

        return self._request(
            "POST",
            f"/tables/{self._quote(name)}",
            json=body,
        )

    def list_tables(self) -> list[dict[str, Any]]:
        """List all tables."""
        return self._request("GET", "/tables")

    def get_table(
        self,
        name: str,
    ) -> dict[str, Any]:
        """Get table details."""
        return self._request(
            "GET",
            f"/tables/{self._quote(name)}",
        )

    def drop_table(self, name: str) -> None:
        """Drop a table."""
        self._request(
            "DELETE",
            f"/tables/{self._quote(name)}",
        )

    # --------------------------------------------------------------------- #
    # Data operations
    # --------------------------------------------------------------------- #

    def get(
        self,
        table: str,
        key: str,
    ) -> dict[str, Any]:
        """
        Get record by key.
        """
        response = lookup_key.sync(
            table_name=table,
            key=key,
            client=self._client,
        )

        self._raise_if_error(
            response,
            f"Failed to get key '{key}' "
            f"from table '{table}'",
        )

        return response.to_dict()

    def batch(
        self,
        table: str,
        *,
        inserts: dict[str, dict[str, Any]] | None = None,
        deletes: list[str] | None = None,
    ) -> None:
        """
        Perform batch operations.
        """
        request = BatchRequest(
            inserts=(
                inserts
                if inserts is not None
                else UNSET
            ),
            deletes=deletes or [],
        )

        response = batch.sync(
            table_name=table,
            client=self._client,
            body=request,
        )

        self._raise_if_error(
            response,
            f"Batch operation failed "
            f"for table '{table}'",
        )

    # --------------------------------------------------------------------- #
    # Shared response handling
    # --------------------------------------------------------------------- #

    @staticmethod
    def _raise_if_error(
        response: Any,
        message: str,
    ) -> None:
        """Raise AntflyException if response contains error."""
        if response is None:
            raise AntflyException(message)

        if isinstance(response, Error):
            raise AntflyException(
                f"{message}: {response.error}"
            )

    # --------------------------------------------------------------------- #
    # Context manager support
    # --------------------------------------------------------------------- #

    def close(self) -> None:
        """Close underlying HTTP client."""
        self._httpx_client.close()

    def __enter__(self) -> "AntflyClient":
        return self

    def __exit__(
        self,
        exc_type: Any,
        exc: Any,
        tb: Any,
    ) -> None:
        self.close()

    # --------------------------------------------------------------------- #
    # Representation
    # --------------------------------------------------------------------- #

    def __repr__(self) -> str:
        return (
            f"{self.__class__.__name__}("
            f"base_url={self.base_url!r})"
        )
