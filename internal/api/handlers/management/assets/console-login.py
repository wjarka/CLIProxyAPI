# /// script
# requires-python = ">=3.11"
# dependencies = ["playwright>=1.56,<2"]
# ///
"""Import one explicitly authorized Console browser session into CLIProxyAPI.

Run with uv: uv run console-login.py --provider claude --url https://proxy.example
Each invocation uses a new isolated browser context; repeat for separate logins.
No browser profile, password, session token, or management key is printed/saved here.
"""
import argparse
import asyncio
import getpass
import ipaddress
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


def management_url(raw: str) -> str:
    parsed = urllib.parse.urlsplit(raw.strip())
    # Parsing the port also validates malformed/non-numeric/out-of-range ports.
    _ = parsed.port
    host = parsed.hostname or ""
    try:
        loopback = ipaddress.ip_address(host).is_loopback
    except ValueError:
        loopback = host.lower() == "localhost"
    if (
        not host
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or (parsed.scheme != "https" and not (parsed.scheme == "http" and loopback))
    ):
        raise ValueError("Use an HTTPS management URL (HTTP is allowed only on loopback).")
    path = parsed.path.rstrip("/")
    if path.endswith("/v8/management"):
        path = path[: -len("/v8/management")]
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, path, "", ""))


def claude_session(cookies: list[dict]) -> str:
    for cookie in cookies:
        domain = cookie.get("domain", "").lstrip(".").lower()
        if (
            cookie.get("name") == "sessionKey"
            and domain in {"claude.com", "platform.claude.com"}
            and cookie.get("value")
        ):
            return cookie["value"]
    raise ValueError("No Claude Console session found. Finish signing in and try again.")


def openai_session(url: str, headers: dict[str, str]) -> str | None:
    parsed = urllib.parse.urlsplit(url)
    if (
        parsed.scheme != "https"
        or parsed.netloc not in {"api.openai.com", "platform.openai.com"}
        or not parsed.path.startswith(("/dashboard/", "/v1/dashboard/"))
    ):
        return None
    authorization = next((value for key, value in headers.items() if key.lower() == "authorization"), "")
    scheme, _, token = authorization.partition(" ")
    token = token.strip()
    if scheme.lower() != "bearer" or not token or token.startswith("sk-") or any(c.isspace() for c in token):
        return None
    return token


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(newurl, code, "Redirect refused", headers, fp)


def upload_connection(base: str, key: str, provider: str, label: str, session: str, connection_id: str = "") -> str:
    base = management_url(base)
    path = "/v8/management/billing/connections"
    payload = {"provider": provider, "label": label, "session": session}
    method = "POST"
    if connection_id:
        path += "/" + urllib.parse.quote(connection_id, safe="")
        payload = {"session": session}
        method = "PATCH"
    request = urllib.request.Request(
        base + path,
        data=json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json", "Accept": "application/json"},
        method=method,
    )
    opener = urllib.request.build_opener(NoRedirect())
    with opener.open(request) as response:
        result = json.loads(response.read(1 << 20))
    identifier = result.get("connection", {}).get("id")
    if not isinstance(identifier, str) or not identifier:
        raise ValueError("The server returned an invalid connection response.")
    return identifier


async def browser_login(provider: str) -> str:
    from playwright.async_api import async_playwright

    async with async_playwright() as playwright:
        try:
            browser = await playwright.chromium.launch(channel="chrome", headless=False)
        except Exception:
            try:
                browser = await playwright.chromium.launch(headless=False)
            except Exception:
                raise ValueError("Chrome or Playwright Chromium is required. Install Chrome, or run: uv run --with playwright playwright install chromium") from None
        async with browser:
            context = await browser.new_context()
            token = ""

            async def capture(request):
                nonlocal token
                try:
                    candidate = openai_session(request.url, await request.all_headers())
                    if candidate:
                        token = candidate
                except Exception:
                    # A completed/cancelled browser request is not a login failure.
                    pass

            if provider == "openai":
                context.on("request", capture)
            page = await context.new_page()
            target = (
                "https://platform.claude.com/settings/billing"
                if provider == "claude"
                else "https://platform.openai.com/settings/organization/billing/overview"
            )
            await page.goto(target)
            while True:
                await asyncio.to_thread(input, "Sign in to the intended Console account, open Billing, then press Enter here: ")
                if provider == "claude":
                    try:
                        return claude_session(await context.cookies("https://platform.claude.com"))
                    except ValueError:
                        print("The Console login is not ready yet.", file=sys.stderr)
                elif token:
                    return token
                else:
                    print("No OpenAI dashboard session captured yet. Open or refresh Billing in the browser.", file=sys.stderr)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", choices=("claude", "openai"), required=True)
    parser.add_argument("--url", required=True, help="CLIProxyAPI management origin, including any path prefix")
    parser.add_argument("--label", default="", help="Name for a new connection")
    parser.add_argument("--connection-id", default="", help="Reconnect an existing connection")
    args = parser.parse_args()
    try:
        base = management_url(args.url)
        label = args.label or (input("Connection label: ").strip() if not args.connection_id else "")
        if not args.connection_id and not label:
            raise ValueError("A connection label is required.")
        key = os.environ.get("CLIPROXY_MANAGEMENT_KEY") or getpass.getpass("CLIProxyAPI management key: ")
        if not key.strip():
            raise ValueError("A management key is required.")
        session = asyncio.run(browser_login(args.provider))
        identifier = upload_connection(base, key, args.provider, label, session, args.connection_id)
        session = key = ""
        print("Console connection saved: " + identifier)
        return 0
    except (KeyboardInterrupt, EOFError):
        print("Login cancelled.", file=sys.stderr)
    except urllib.error.HTTPError as error:
        print(f"The server refused the connection (HTTP {error.code}). Check the Billing page and management key.", file=sys.stderr)
    except urllib.error.URLError:
        print("Could not reach the management server.", file=sys.stderr)
    except ValueError as error:
        print(str(error), file=sys.stderr)
    except Exception:
        print("Console login failed. Close the browser and try again.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
