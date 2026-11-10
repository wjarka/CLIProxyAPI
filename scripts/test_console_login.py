"""Offline security and upload-contract tests for the downloadable login helper."""
import asyncio
import importlib.util
import json
import pathlib
import sys
from types import ModuleType, SimpleNamespace
import unittest
from unittest.mock import patch

ASSET = pathlib.Path(__file__).resolve().parents[1] / "internal/api/handlers/management/assets/console-login.py"
spec = importlib.util.spec_from_file_location("console_login", ASSET)
login = importlib.util.module_from_spec(spec)
spec.loader.exec_module(login)


class ConsoleLoginTests(unittest.TestCase):
    def test_management_url_requires_tls_except_loopback(self):
        self.assertEqual(login.management_url("https://proxy.test/v8/management/"), "https://proxy.test")
        self.assertEqual(login.management_url("http://127.0.0.1:8317"), "http://127.0.0.1:8317")
        self.assertEqual(login.management_url("https://proxy.test/mount"), "https://proxy.test/mount")
        for value in ["http://proxy.test", "https://user:key@proxy.test", "https://proxy.test?key=x", "https://proxy.test/#x", "file:///tmp/key", "https://proxy.test:bad"]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                login.management_url(value)

    def test_claude_cookie_is_bound_to_console_domain(self):
        self.assertEqual(login.claude_session([{"name": "sessionKey", "value": "private-session", "domain": ".claude.com"}]), "private-session")
        for domain in ["evil.test", "claude.com.evil.test"]:
            with self.assertRaises(ValueError):
                login.claude_session([{"name": "sessionKey", "value": "secret", "domain": domain}])

    def test_openai_capture_accepts_only_dashboard_sessions_on_fixed_hosts(self):
        self.assertEqual(login.openai_session("https://api.openai.com/dashboard/billing/credit_grants", {"authorization": "Bearer sess-private"}), "sess-private")
        for url, token in [
            ("https://evil.test/dashboard/billing", "sess-secret"),
            ("https://api.openai.com.evil.test/dashboard/billing", "sess-secret"),
            ("http://api.openai.com/dashboard/billing", "sess-secret"),
            ("https://api.openai.com/v1/responses", "sess-secret"),
            ("https://api.openai.com/dashboard/billing", "sk-proj-inference-key"),
        ]:
            self.assertIsNone(login.openai_session(url, {"authorization": "Bearer " + token}))

    def test_upload_keeps_secrets_in_request_body_and_auth_header(self):
        class Response:
            def __enter__(self): return self
            def __exit__(self, *args): return None
            def read(self, *args): return b'{"connection":{"id":"connection-1"}}'
        captured = []
        class Opener:
            def open(self, request):
                captured.append(request)
                return Response()
        with patch.object(login.urllib.request, "build_opener", return_value=Opener()):
            result = login.upload_connection("https://proxy.test", "management-secret", "claude", "Personal", "session-secret")
            login.upload_connection("https://proxy.test", "management-secret", "claude", "Personal", "new-session", "connection-1")
        self.assertEqual(result, "connection-1")
        self.assertEqual(captured[0].full_url, "https://proxy.test/v8/management/billing/connections")
        self.assertEqual(captured[0].get_header("Authorization"), "Bearer management-secret")
        self.assertEqual(json.loads(captured[0].data), {"provider": "claude", "label": "Personal", "session": "session-secret"})
        self.assertEqual(captured[1].method, "PATCH")
        self.assertEqual(captured[1].full_url, "https://proxy.test/v8/management/billing/connections/connection-1")
        self.assertEqual(json.loads(captured[1].data), {"session": "new-session"})

    def test_upload_never_follows_redirects(self):
        with self.assertRaises(login.urllib.error.HTTPError) as caught:
            login.NoRedirect().redirect_request(None, None, 302, "redirect", {}, "https://evil.test/")
        caught.exception.close()


class OfflineBrowser:
    """Only browser I/O is replaced; browser_login and its callback run normally."""

    def __init__(self, cookies=()):
        self.cookie_values = list(cookies)
        self.handlers = {}
        self.contexts = []
        self.visited = []
        self.cookie_origins = []
        self.closed = False

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        self.closed = True

    async def new_context(self):
        context = SimpleNamespace(
            on=lambda event, callback: self.handlers.__setitem__(event, callback),
            new_page=self.new_page,
            cookies=self.cookies,
        )
        self.contexts.append(context)
        return context

    async def new_page(self):
        return SimpleNamespace(goto=self.goto)

    async def goto(self, url):
        self.visited.append(url)

    async def cookies(self, url):
        self.cookie_origins.append(url)
        return self.cookie_values


class OfflinePlaywright:
    def __init__(self, cookies=()):
        self.cookies = cookies
        self.browsers = []
        self.chromium = self

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        return None

    async def launch(self, *, channel=None, headless=True):
        if headless:
            raise AssertionError("Interactive Console login must show a browser")
        browser = OfflineBrowser(self.cookies)
        self.browsers.append(browser)
        return browser

    def modules(self):
        package = ModuleType("playwright")
        api = ModuleType("playwright.async_api")
        api.async_playwright = lambda: self
        package.async_api = api
        return {"playwright": package, "playwright.async_api": api}


class BrowserLoginTests(unittest.IsolatedAsyncioTestCase):
    async def test_claude_acquires_cookie_in_three_isolated_logins_and_closes_browsers(self):
        browser = OfflinePlaywright([
            {"name": "sessionKey", "value": "fixture-console-session", "domain": ".claude.com"}
        ])
        with patch.dict(sys.modules, browser.modules()), patch("builtins.input", return_value=""):
            results = [await login.browser_login("claude") for _ in range(3)]
        self.assertEqual(results, ["fixture-console-session"] * 3)
        self.assertEqual(len(browser.browsers), 3)
        contexts = [item.contexts[0] for item in browser.browsers]
        self.assertEqual(len({id(context) for context in contexts}), 3)
        for item in browser.browsers:
            self.assertTrue(item.closed)
            self.assertEqual(item.visited, ["https://platform.claude.com/settings/billing"])
            self.assertEqual(item.cookie_origins, ["https://platform.claude.com"])
            self.assertNotIn("request", item.handlers)

    async def test_openai_captures_request_while_console_input_waits_and_closes_browser(self):
        browser = OfflinePlaywright()
        loop = asyncio.get_running_loop()
        prompts = 0

        class DashboardRequest:
            url = "https://api.openai.com/v1/dashboard/billing/credit_grants"

            async def all_headers(self):
                return {"authorization": "Bearer sess-fixture-dashboard"}

        def enter_after_browser_request(prompt):
            nonlocal prompts
            prompts += 1
            if prompts != 1:
                raise AssertionError("Dashboard request did not produce a captured session")
            # The input thread waits for a real coroutine on the browser's event loop.
            # Synchronous input() on that loop stalls it and fails this watchdog.
            capture = browser.browsers[0].handlers["request"]
            task = asyncio.run_coroutine_threadsafe(capture(DashboardRequest()), loop)
            try:
                task.result(timeout=2)
            except TimeoutError as error:
                task.cancel()
                raise AssertionError("Browser events stalled while Console input waited") from error
            return ""

        with patch.dict(sys.modules, browser.modules()), patch("builtins.input", enter_after_browser_request):
            result = await login.browser_login("openai")
        self.assertEqual(result, "sess-fixture-dashboard")
        self.assertEqual(browser.browsers[0].visited, [
            "https://platform.openai.com/settings/organization/billing/overview"
        ])
        self.assertTrue(browser.browsers[0].closed)

    async def test_cancelled_console_input_closes_browser(self):
        browser = OfflinePlaywright()
        with patch.dict(sys.modules, browser.modules()), patch("builtins.input", side_effect=EOFError):
            with self.assertRaises(EOFError):
                await login.browser_login("claude")
        self.assertTrue(browser.browsers[0].closed)


if __name__ == "__main__":
    unittest.main()
