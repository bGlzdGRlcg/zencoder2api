const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

function fakeElement(id = "") {
	const classes = new Set();
	return {
		id,
		attributes: {},
		checked: false,
		children: [],
		classList: {
			add: (...names) => names.forEach((name) => classes.add(name)),
			contains: (name) => classes.has(name),
			remove: (...names) => names.forEach((name) => classes.delete(name)),
			toggle: (name, force) => {
				const enabled = force === undefined ? !classes.has(name) : force;
				if (enabled) classes.add(name);
				else classes.delete(name);
				return enabled;
			},
		},
		dataset: {},
		disabled: false,
		innerHTML: "",
		isConnected: true,
		parentElement: null,
		style: {},
		textContent: "",
		type: id === "adminPassword" ? "password" : "",
		value: "",
		addEventListener() {},
		appendChild(child) {
			this.children.push(child);
			child.parentElement = this;
		},
		closest() {
			return null;
		},
		focus() {
			this.focused = true;
		},
		querySelector() {
			return null;
		},
		remove() {},
		removeEventListener() {},
		replaceChildren() {},
		scrollIntoView() {},
		select() {},
		setAttribute(name, value) {
			this.attributes[name] = String(value);
		},
		removeAttribute(name) {
			delete this.attributes[name];
		},
		setSelectionRange() {},
	};
}

function createHarness(fetch, options = {}) {
	const elements = new Map();
	const getElement = (id) => {
		if (!elements.has(id)) elements.set(id, fakeElement(id));
		return elements.get(id);
	};
	const statusDot = fakeElement("accountsStatusDot");
	statusDot.parentElement = getElement("accountsRefreshIndicator");
	const document = {
		activeElement: fakeElement("activeElement"),
		body: fakeElement("body"),
		documentElement: fakeElement("html"),
		readyState: "loading",
		addEventListener() {},
		createElement: () => fakeElement(),
		execCommand: () => true,
		getElementById: getElement,
		querySelector: (selector) =>
			selector === "[data-accounts-status-dot]" ? statusDot : null,
		querySelectorAll: () => [],
	};
	const context = {
		AbortController: options.AbortController || AbortController,
		AbortSignal: options.AbortSignal || AbortSignal,
		DOMException,
		Headers,
		Response,
		TextEncoder,
		URL,
		URLSearchParams,
		alert() {},
		clearTimeout,
		confirm: options.confirm || (() => true),
		console: { error() {}, log() {}, warn() {} },
		document,
		fetch,
		localStorage: {},
		navigator: options.navigator || {},
		setTimeout,
		window: {
			addEventListener() {},
			history: { replaceState() {} },
			isSecureContext: options.isSecureContext || false,
			location: { href: "http://localhost/" },
			matchMedia: () => ({ matches: false }),
		},
	};
	context.globalThis = context;
	vm.createContext(context);
	const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");
	vm.runInContext(
		`${source}\nglobalThis.__appTest = { createAdminSession, loadAccounts, readResponseError, submitAPIKey, beginAPIKeyRotation, startZencoderOAuth, completeZencoderOAuth, resetOAuthFlow, deleteAccount, batchDelete, currentState, logout, getOAuthFlow: () => oauthFlow, activateSession: () => { appSessionActive = true; }, getUsageCreditsView, usageCreditsRefreshButton, refreshCreditsForAccount, refreshAllCredits, renderAccounts };`,
		context,
	);
	return { app: context.__appTest, document, elements, getElement };
}

function account(id) {
	return {
		id,
		credential_type: "oauth",
		oauth_email: `account-${id}@example.test`,
		last_used: "0001-01-01T00:00:00Z",
		daily_used: 0,
		total_used: 0,
	};
}

function usageCredits(overrides = {}) {
	return {
		state: "ready",
		revision: 1,
		operation_credits: 8,
		operation_exists: true,
		turns: 1,
		consumed: 8,
		budget: 5000,
		remaining: 4992,
		updated_at: "2026-07-22T00:00:00Z",
		...overrides,
	};
}

test("a higher credit revision wins even when its server clock is older", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve) => {
			requests.push({ url, method: options.method || "GET", resolve });
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{
			...account(1),
			usage_based_credits: usageCredits({
				remaining: 4990,
				revision: 2,
				updated_at: "2026-07-22T00:02:00Z",
			}),
		},
	];

	const load = app.loadAccounts();
	requests[0].resolve(
		new Response(
			JSON.stringify({
				items: [
					{
						...account(1),
						usage_based_credits: usageCredits({
							remaining: 4980,
							revision: 3,
							updated_at: "2026-07-22T00:01:00Z",
						}),
					},
				],
				total: 1,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await load;

	assert.equal(app.currentState.items[0].usage_based_credits.remaining, 4980);
});

test("a newer page request cancels and supersedes the old response", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			options.signal.addEventListener(
				"abort",
				() => reject(new DOMException("Aborted", "AbortError")),
				{ once: true },
			);
			requests.push({ url, resolve });
		});
	const { app } = createHarness(fetch, { AbortSignal: {} });
	app.activateSession();
	app.currentState.size = 1;
	app.currentState.total = 2;

	const first = app.loadAccounts();
	app.currentState.page = 2;
	const second = app.loadAccounts();
	requests[1].resolve(
		new Response(JSON.stringify({ items: [account(2)], total: 2 }), {
			headers: { "Content-Type": "application/json" },
		}),
	);

	await Promise.all([first, second]);
	assert.match(requests[0].url, /page=1/);
	assert.match(requests[1].url, /page=2/);
	assert.equal(app.currentState.page, 2);
	assert.equal(app.currentState.items[0].id, 2);
});

test("an out-of-range page is clamped and reloaded", async () => {
	const urls = [];
	const { app } = createHarness(async (url) => {
		urls.push(url);
		const page = new URL(url, "http://localhost").searchParams.get("page");
		return new Response(
			JSON.stringify({
				items: page === "1" ? [account(1)] : [],
				total: 1,
			}),
			{ headers: { "Content-Type": "application/json" } },
		);
	});
	app.activateSession();
	app.currentState.page = 2;
	app.currentState.size = 1;
	app.currentState.total = 2;

	await app.loadAccounts();
	assert.deepEqual(urls.map((url) => new URL(url, "http://localhost").searchParams.get("page")), ["2", "1"]);
	assert.equal(app.currentState.page, 1);
	assert.equal(app.currentState.items[0].id, 1);
});

test("invalid API keys remain editable and never reach the API", async () => {
	let fetchCount = 0;
	const { app, getElement } = createHarness(async () => {
		fetchCount += 1;
		throw new Error("unexpected request");
	});
	app.activateSession();
	const input = getElement("apiKeyValue");
	input.value = "short";

	await app.submitAPIKey({ preventDefault() {} });
	assert.equal(fetchCount, 0);
	assert.equal(input.value, "short");
	assert.equal(input.focused, true);
	assert.equal(getElement("apiKeyStatus").textContent, "API Key 必须为 16 至 4096 字节");
	assert.equal(input.attributes["aria-invalid"], "true");
	assert.equal(getElement("apiKeyStatus").attributes.role, "alert");
});

test("API-key rotation hides the previous credential's Credit when reload fails", async () => {
	const requests = [];
	const fetch = async (url, options) => {
		requests.push({ url, method: options.method || "GET" });
		if (url === "/api/accounts/credits/refresh") {
			return new Response(
				JSON.stringify({
					items: [
						{
							id: 1,
							usage_based_credits: usageCredits({ revision: 20, remaining: 4700 }),
						},
					],
				}),
				{ headers: { "Content-Type": "application/json" } },
			);
		}
		if (url === "/api/accounts/1/api-key") {
			return new Response(JSON.stringify({ message: "API key rotated" }), {
				headers: { "Content-Type": "application/json" },
			});
		}
		if (url.startsWith("/api/accounts?")) {
			return new Response(JSON.stringify({ error: "temporary failure" }), {
				status: 503,
				headers: { "Content-Type": "application/json" },
			});
		}
		throw new Error(`unexpected request: ${url}`);
	};
	const { app, getElement } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{ ...account(1), credential_type: "api_key", usage_based_credits: usageCredits() },
	];

	await app.refreshCreditsForAccount(1);
	assert.equal(app.currentState.items[0].usage_based_credits.revision, 20);
	app.beginAPIKeyRotation(1);
	getElement("apiKeyValue").value = "rotated-api-key-5678";
	await app.submitAPIKey({ preventDefault() {} });

	assert.deepEqual(requests.map(({ method }) => method), ["POST", "PUT", "GET"]);
	assert.equal(app.currentState.items[0].usage_based_credits.state, "unknown");
	assert.equal(app.currentState.items[0].usage_based_credits.revision, 21);
});

test("API-key rotation ignores a delayed Credit refresh from the previous key", async () => {
	let resolveRefresh;
	let refreshSignal;
	const fetch = (url, options) => {
		if (url === "/api/accounts/credits/refresh") {
			refreshSignal = options.signal;
			return new Promise((resolve) => {
				resolveRefresh = resolve;
			});
		}
		if (url === "/api/accounts/1/api-key") {
			return Promise.resolve(new Response(null, { status: 200 }));
		}
		if (url.startsWith("/api/accounts?")) {
			return Promise.resolve(new Response(null, { status: 503 }));
		}
		throw new Error(`unexpected request: ${url}`);
	};
	const { app, getElement } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{
			...account(1),
			credential_type: "api_key",
			usage_based_credits: usageCredits({ revision: 1 }),
		},
	];

	const refresh = app.refreshCreditsForAccount(1);
	app.beginAPIKeyRotation(1);
	getElement("apiKeyValue").value = "rotated-api-key-5678";
	await app.submitAPIKey({ preventDefault() {} });
	assert.equal(refreshSignal.aborted, true);

	resolveRefresh(
		new Response(
			JSON.stringify({
				items: [
					{
						id: 1,
						usage_based_credits: usageCredits({ revision: 20, remaining: 4700 }),
					},
				],
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await refresh;

	assert.equal(app.currentState.items[0].usage_based_credits.state, "unknown");
	assert.equal(app.currentState.items[0].usage_based_credits.revision, 2);
});

test("successful OAuth re-login clears local Credit snapshots before reload", async () => {
	const fetch = async (url, options = {}) => {
		if (url === "/api/accounts/credits/refresh") {
			return new Response(
				JSON.stringify({
					items: [{ id: 1, usage_based_credits: usageCredits({ revision: 20, remaining: 4700 }) }],
				}),
				{ headers: { "Content-Type": "application/json" } },
			);
		}
		if (url === "/api/oauth/zencoder/start") {
			return new Response(
				JSON.stringify({
					authorization_url: "https://zencoder.example.test/auth/state-a",
					state: "state-a",
				}),
				{ headers: { "Content-Type": "application/json" } },
			);
		}
		if (url === "/api/oauth/zencoder/complete") {
			return new Response(JSON.stringify({ message: "连接成功" }), {
				headers: { "Content-Type": "application/json" },
			});
		}
		if (url.startsWith("/api/accounts?")) {
			return new Response(JSON.stringify({ error: "temporary failure" }), {
				status: 503,
				headers: { "Content-Type": "application/json" },
			});
		}
		throw new Error(`unexpected request: ${options.method || "GET"} ${url}`);
	};
	const { app, getElement } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [{ ...account(1), usage_based_credits: usageCredits() }];
	await app.refreshCreditsForAccount(1);
	assert.equal(app.currentState.items[0].usage_based_credits.revision, 20);

	await app.startZencoderOAuth();
	const flow = app.getOAuthFlow();
	getElement("oauthCallbackURL").value =
		`http://localhost/oauth/zencoder/callback/${flow.state}?code=code-a`;
	await app.completeZencoderOAuth({ preventDefault() {} });

	assert.equal(app.currentState.items[0].usage_based_credits.state, "unknown");
	assert.equal(app.currentState.items[0].usage_based_credits.revision, 21);
});

test("a successful login response must contain the session contract", async () => {
	const { app } = createHarness(async () =>
		new Response(JSON.stringify({}), {
			headers: { "Content-Type": "application/json" },
		}),
	);
	assert.equal(await app.createAdminSession("valid-password"), false);
});

test("nested API errors are rendered as readable messages", async () => {
	const { app } = createHarness(async () => new Response());
	const response = new Response(
		JSON.stringify({ error: { message: "管理员会话已失效" } }),
		{ status: 401, headers: { "Content-Type": "application/json" } },
	);
	assert.equal(
		await app.readResponseError(response, "请求失败"),
		"管理员会话已失效",
	);
});

test("logout aborts a pending API-key mutation without stale UI feedback", async () => {
	let pendingRequest;
	const fetch = (url, options) => {
		if (url === "/api/accounts/api-key") {
			return new Promise((resolve, reject) => {
				pendingRequest = { options, resolve, reject };
				options.signal.addEventListener(
					"abort",
					() => reject(new DOMException("Aborted", "AbortError")),
					{ once: true },
				);
			});
		}
		return Promise.resolve(new Response(null, { status: 204 }));
	};
	const { app, getElement } = createHarness(fetch);
	app.activateSession();
	getElement("apiKeyValue").value = "1234567890abcdef";
	getElement("adminPassword").type = "text";

	const submit = app.submitAPIKey({ preventDefault() {} });
	assert.ok(pendingRequest, "API-key request did not start");
	const logout = app.logout();
	await Promise.all([submit, logout]);

	assert.equal(pendingRequest.options.signal.aborted, true);
	assert.equal(getElement("apiKeyStatus").textContent, "");
	assert.equal(getElement("apiKeySubmit").disabled, false);
	assert.equal(getElement("apiKeySubmitLoading").classList.contains("hidden"), true);
	assert.equal(getElement("adminPassword").type, "password");
});

test("logout freezes the admin UI and clears account state before DELETE completes", async () => {
	let resolveLogout;
	let mutationRequests = 0;
	const fetch = (url) => {
		if (url === "/api/admin/session") {
			return new Promise((resolve) => {
				resolveLogout = resolve;
			});
		}
		mutationRequests += 1;
		return Promise.resolve(new Response(null, { status: 204 }));
	};
	const { app, getElement } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [account(1)];
	app.currentState.total = 1;
	app.currentState.selectedIds.add(1);
	getElement("apiKeyValue").value = "1234567890abcdef";

	const logout = app.logout();
	assert.equal(getElement("mainApp").classList.contains("hidden"), true);
	assert.equal(getElement("adminPassword").disabled, true);
	assert.equal(
		getElement("adminPasswordModal").attributes["aria-busy"],
		"true",
	);
	assert.equal(getElement("adminPasswordModal").focused, true);
	assert.equal(app.currentState.items.length, 0);
	assert.equal(app.currentState.total, 0);
	assert.equal(app.currentState.selectedIds.size, 0);

	await app.submitAPIKey({ preventDefault() {} });
	assert.equal(mutationRequests, 0);

	resolveLogout(new Response(null, { status: 204 }));
	await logout;
	assert.equal(getElement("adminPassword").disabled, false);
	assert.equal(
		getElement("adminPasswordModal").attributes["aria-busy"],
		undefined,
	);
});

test("a pending OAuth completion blocks another copy until it settles", async () => {
	let startCount = 0;
	let clipboardWrites = 0;
	let resolveCompletion;
	const fetch = (url) => {
		if (url === "/api/oauth/zencoder/start") {
			startCount += 1;
			const state = `state-${startCount}`;
			return Promise.resolve(
				new Response(
					JSON.stringify({
						authorization_url: `https://zencoder.example.test/auth/${state}`,
						state,
					}),
					{ headers: { "Content-Type": "application/json" } },
				),
			);
		}
		if (url === "/api/oauth/zencoder/complete") {
			return new Promise((resolve) => {
				resolveCompletion = resolve;
			});
		}
		if (url.startsWith("/api/accounts?")) {
			return Promise.resolve(
				new Response(JSON.stringify({ items: [], total: 0 }), {
					headers: { "Content-Type": "application/json" },
				}),
			);
		}
		throw new Error(`unexpected request: ${url}`);
	};
	const { app, getElement } = createHarness(fetch, {
		isSecureContext: true,
		navigator: {
			clipboard: {
				async writeText() {
					clipboardWrites += 1;
				},
			},
		},
	});
	app.activateSession();

	await app.startZencoderOAuth();
	const flowA = app.getOAuthFlow();
	getElement("oauthCallbackURL").value =
		`http://localhost/oauth/zencoder/callback/${flowA.state}?code=code-a`;
	const completion = app.completeZencoderOAuth({ preventDefault() {} });
	assert.ok(resolveCompletion, "OAuth completion request did not start");
	assert.equal(getElement("oauthBtn").disabled, true);
	assert.equal(getElement("oauthCompleteBtn").disabled, true);

	await app.startZencoderOAuth();
	assert.equal(startCount, 1);
	assert.equal(clipboardWrites, 1);
	assert.equal(app.getOAuthFlow().id, flowA.id);

	resolveCompletion(
		new Response(JSON.stringify({ message: "连接成功" }), {
			headers: { "Content-Type": "application/json" },
		}),
	);
	await completion;

	assert.equal(app.getOAuthFlow(), null);
	assert.equal(getElement("oauthBtn").disabled, false);
	assert.equal(getElement("oauthCompleteBtn").disabled, false);
	await app.startZencoderOAuth();
	assert.equal(startCount, 2);
	assert.equal(clipboardWrites, 2);
	assert.equal(getElement("oauthBtnText").textContent, "已复制 · 再次复制");
	app.resetOAuthFlow();
});

test("OAuth completion pauses and restores the remaining flow lifetime", async () => {
	let resolveCompletion;
	const fetch = (url) => {
		if (url === "/api/oauth/zencoder/start") {
			return Promise.resolve(
				new Response(
					JSON.stringify({
						authorization_url: "https://zencoder.example.test/auth/state-a",
						state: "state-a",
					}),
					{ headers: { "Content-Type": "application/json" } },
				),
			);
		}
		if (url === "/api/oauth/zencoder/complete") {
			return new Promise((resolve) => {
				resolveCompletion = resolve;
			});
		}
		throw new Error(`unexpected request: ${url}`);
	};
	const { app, getElement } = createHarness(fetch);
	app.activateSession();

	await app.startZencoderOAuth();
	const flow = app.getOAuthFlow();
	flow.expiresAt = Date.now() + 60_000;
	getElement("oauthCallbackURL").value =
		`http://localhost/oauth/zencoder/callback/${flow.state}?code=code-a`;
	const completion = app.completeZencoderOAuth({ preventDefault() {} });

	assert.ok(resolveCompletion, "OAuth completion request did not start");
	assert.equal(flow.timeoutTimer, null);
	resolveCompletion(
		new Response(JSON.stringify({ error: "上游暂时不可用" }), {
			status: 502,
			headers: { "Content-Type": "application/json" },
		}),
	);
	await completion;

	assert.equal(app.getOAuthFlow().id, flow.id);
	assert.notEqual(flow.timeoutTimer, null);
	assert.equal(getElement("oauthCallbackError").textContent, "上游暂时不可用");
	app.resetOAuthFlow();
});

test("a late clipboard write cannot mark an expired OAuth flow as copied", async () => {
	let startCount = 0;
	const writes = [];
	const fetch = () => {
		startCount += 1;
		const state = `state-${startCount}`;
		return Promise.resolve(
			new Response(
				JSON.stringify({
					authorization_url: `https://zencoder.example.test/auth/${state}`,
					state,
				}),
				{ headers: { "Content-Type": "application/json" } },
			),
		);
	};
	const clipboard = {
		writeText(value) {
			return new Promise((resolve) => writes.push({ resolve, value }));
		},
	};
	const { app, getElement } = createHarness(fetch, {
		isSecureContext: true,
		navigator: { clipboard },
	});
	app.activateSession();

	const first = app.startZencoderOAuth();
	for (let attempt = 0; writes.length < 1 && attempt < 20; attempt += 1) {
		await new Promise((resolve) => setImmediate(resolve));
	}
	assert.equal(writes.length, 1, "first clipboard write did not start");
	app.resetOAuthFlow();
	writes[0].resolve();
	await first;
	assert.equal(app.getOAuthFlow(), null);
	assert.equal(getElement("oauthBtnText").textContent, "复制授权链接");

	const second = app.startZencoderOAuth();
	for (let attempt = 0; writes.length < 2 && attempt < 20; attempt += 1) {
		await new Promise((resolve) => setImmediate(resolve));
	}
	assert.equal(writes.length, 2, "second clipboard write did not start");
	writes[1].resolve();
	await second;
	const flowB = app.getOAuthFlow();

	assert.equal(app.getOAuthFlow().id, flowB.id);
	assert.equal(getElement("oauthBtnText").textContent, "已复制 · 再次复制");
	app.resetOAuthFlow();
});

test("a pending OAuth copy blocks callback submission", async () => {
	let completeRequests = 0;
	let resolveClipboard;
	const fetch = (url) => {
		if (url === "/api/oauth/zencoder/start") {
			return Promise.resolve(
				new Response(
					JSON.stringify({
						authorization_url: "https://zencoder.example.test/auth/state-a",
						state: "state-a",
					}),
					{ headers: { "Content-Type": "application/json" } },
				),
			);
		}
		if (url === "/api/oauth/zencoder/complete") {
			completeRequests += 1;
			return Promise.resolve(new Response(null, { status: 204 }));
		}
		throw new Error(`unexpected request: ${url}`);
	};
	const { app, getElement } = createHarness(fetch, {
		isSecureContext: true,
		navigator: {
			clipboard: {
				writeText() {
					return new Promise((resolve) => {
						resolveClipboard = resolve;
					});
				},
			},
		},
	});
	app.activateSession();

	const copy = app.startZencoderOAuth();
	for (let attempt = 0; !resolveClipboard && attempt < 20; attempt += 1) {
		await new Promise((resolve) => setImmediate(resolve));
	}
	assert.ok(resolveClipboard, "clipboard write did not start");
	const flow = app.getOAuthFlow();
	getElement("oauthCallbackURL").value =
		`http://localhost/oauth/zencoder/callback/${flow.state}?code=code-a`;

	await app.completeZencoderOAuth({ preventDefault() {} });
	assert.equal(completeRequests, 0);
	assert.equal(getElement("oauthBtn").disabled, true);
	assert.equal(getElement("oauthCompleteBtn").disabled, true);

	resolveClipboard();
	await copy;
	assert.equal(getElement("oauthBtn").disabled, false);
	assert.equal(getElement("oauthCompleteBtn").disabled, false);
	app.resetOAuthFlow();
});

test("a pending delete blocks duplicate single and batch mutations", async () => {
	let deleteCount = 0;
	let resolveDelete;
	const fetch = (url) => {
		if (url === "/api/accounts/1") {
			deleteCount += 1;
			return new Promise((resolve) => {
				resolveDelete = resolve;
			});
		}
		if (url.startsWith("/api/accounts?")) {
			return Promise.resolve(
				new Response(JSON.stringify({ items: [], total: 0 }), {
					headers: { "Content-Type": "application/json" },
				}),
			);
		}
		throw new Error(`unexpected request: ${url}`);
	};
	const { app } = createHarness(fetch);
	app.activateSession();

	const first = app.deleteAccount(1);
	const duplicate = app.deleteAccount(1);
	const batch = app.batchDelete(true);
	assert.equal(deleteCount, 1);
	assert.ok(resolveDelete, "delete request did not start");

	resolveDelete(new Response(null, { status: 204 }));
	await Promise.all([first, duplicate, batch]);
	assert.equal(deleteCount, 1);
});

test("usage-based credit rendering distinguishes zero from unknown", () => {
	const { app } = createHarness(async () => new Response(null, { status: 204 }));
	const depleted = app.getUsageCreditsView({
		id: 1,
		usage_based_credits: usageCredits({ remaining: 0, consumed: 5000 }),
	});
	assert.match(depleted.primary, /^剩余 0 \/ /);
	assert.match(depleted.primary, /5,000/);

	const unknown = app.getUsageCreditsView({
		id: 2,
		quota_limit: 100,
		quota_used: 100,
		usage_based_credits: {
			state: "unknown",
			remaining: null,
			budget: null,
			consumed: null,
		},
	});
	assert.equal(unknown.primary, "Credit 未知");
});

test("usage-based credit rendering distinguishes stale data from refresh errors", () => {
	const { app } = createHarness(async () => new Response(null, { status: 204 }));
	const stale = app.getUsageCreditsView({
		id: 1,
		usage_based_credits: usageCredits({ state: "stale" }),
	});
	assert.match(stale.primary, /剩余 4,992/);
	assert.match(stale.detail, /数据过旧/);
	assert.doesNotMatch(stale.detail, /刷新失败/);

	const failed = app.getUsageCreditsView({
		id: 2,
		usage_based_credits: usageCredits({ state: "error" }),
	});
	assert.match(failed.primary, /剩余 4,992/);
	assert.match(failed.detail, /刷新失败/);
});

test("usage-based credit rendering includes the billing period reset time", () => {
	const { app } = createHarness(async () => new Response(null, { status: 204 }));
	const view = app.getUsageCreditsView({
		id: 1,
		usage_based_credits: usageCredits({
			period_end: "2026-07-23T09:09:00Z",
		}),
	});
	assert.match(view.detail, /重置于/);
});

test("ready credit data without a timestamp is treated as unknown", () => {
	const { app } = createHarness(async () => new Response(null, { status: 204 }));
	const view = app.getUsageCreditsView({
		id: 1,
		usage_based_credits: usageCredits({ updated_at: undefined }),
	});
	assert.equal(view.primary, "Credit 未知");
});

test("account rows expose the credit value and refresh action on desktop and mobile", () => {
	const { app, getElement } = createHarness(async () => new Response(null, { status: 204 }));
	app.currentState.items = [
		{
			...account(1),
			usage_based_credits: usageCredits({
				remaining: 4992,
				period_end: "2026-07-23T09:09:00Z",
			}),
		},
	];
	app.renderAccounts(app.currentState.items);

	assert.match(getElement("accountList").innerHTML, /剩余 4,992 \/ 5,000/);
	assert.match(getElement("accountList").innerHTML, /重置于/);
	assert.match(getElement("mobileAccountList").innerHTML, /剩余 Credit/);
	assert.match(getElement("mobileAccountList").innerHTML, /重置于/);
	assert.equal(
		(getElement("accountList").innerHTML.match(/data-action="refresh-credits"/g) || []).length,
		1,
	);
	assert.equal(
		(getElement("mobileAccountList").innerHTML.match(/data-action="refresh-credits"/g) || []).length,
		1,
	);
});

test("a single account credit refresh is deduplicated", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({ url, options, resolve, reject });
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [account(1)];

	const first = app.refreshCreditsForAccount(1);
	const duplicate = app.refreshCreditsForAccount(1);
	assert.equal(requests.length, 1);
	assert.equal(await duplicate, undefined);
	assert.deepEqual(JSON.parse(requests[0].options.body), { ids: [1] });

	requests[0].resolve(
		new Response(JSON.stringify({ items: [{ id: 1, usage_based_credits: usageCredits() }] }), {
			headers: { "Content-Type": "application/json" },
		}),
	);
	await first;
	assert.equal(app.currentState.items[0].usage_based_credits.remaining, 4992);
});

test("all-account and single-account credit refreshes are mutually exclusive", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({ url, options, resolve, reject });
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [account(1), account(2)];

	const single = app.refreshCreditsForAccount(1);
	await app.refreshAllCredits();
	assert.equal(requests.length, 1);
	requests[0].resolve(
		new Response(JSON.stringify({ items: [{ id: 1, usage_based_credits: usageCredits() }] }), {
			headers: { "Content-Type": "application/json" },
		}),
	);
	await single;

	const all = app.refreshAllCredits();
	await app.refreshCreditsForAccount(2);
	assert.equal(requests.length, 2);
	assert.deepEqual(JSON.parse(requests[1].options.body), { ids: [1, 2] });
	requests[1].resolve(
		new Response(
			JSON.stringify({
				items: [{ id: 2, usage_based_credits: usageCredits({ remaining: 0 }) }],
				requested: 2,
				refreshed: 1,
				skipped: 1,
				failed: 0,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await all;
});

test("all-account credit refresh uses bounded batches and continues after a failed batch", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({ url, options, resolve, reject });
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.page = 1;
	app.currentState.total = 5;
	app.currentState.items = [1, 2, 3, 4, 5].map(account);

	const refresh = app.refreshAllCredits();
	await new Promise((resolve) => setImmediate(resolve));
	assert.deepEqual(JSON.parse(requests[0].options.body), { ids: [1, 2, 3, 4] });
	requests[0].resolve(
		new Response(JSON.stringify({ error: "upstream unavailable" }), {
			status: 503,
			headers: { "Content-Type": "application/json" },
		}),
	);
	await new Promise((resolve) => setImmediate(resolve));
	assert.equal(requests.length, 2);
	assert.deepEqual(JSON.parse(requests[1].options.body), { ids: [5] });
	requests[1].resolve(
		new Response(
			JSON.stringify({
				items: [{ id: 5, usage_based_credits: usageCredits({ remaining: 4980 }) }],
				requested: 1,
				refreshed: 1,
				skipped: 0,
				failed: 0,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await refresh;

	assert.match(app.getUsageCreditsView(app.currentState.items[0]).primary, /刷新失败/);
	assert.equal(app.currentState.items[4].usage_based_credits.remaining, 4980);
});

test("a server-side refreshing snapshot disables duplicate account refresh", async () => {
	let calls = 0;
	const { app } = createHarness(async () => {
		calls += 1;
		return new Response(null, { status: 500 });
	});
	app.activateSession();
	app.currentState.items = [
		{
			...account(1),
			usage_based_credits: usageCredits({ state: "refreshing" }),
		},
	];

	const button = app.usageCreditsRefreshButton(1, "account-1@example.test");
	assert.match(button, /disabled/);
	assert.match(button, /aria-busy="true"/);
	assert.equal(app.getUsageCreditsView(app.currentState.items[0]).primary, "剩余 4,992 / 5,000");
	await app.refreshCreditsForAccount(1);
	assert.equal(calls, 0);
});

test("credit completion restores focus and is announced by only one live region", async () => {
	const fetch = async () =>
		new Response(
			JSON.stringify({
				items: [{ id: 1, usage_based_credits: usageCredits() }],
				requested: 1,
				refreshed: 1,
				skipped: 0,
				failed: 0,
			}),
			{ headers: { "Content-Type": "application/json" } },
		);
	const { app, document, getElement } = createHarness(fetch);
	app.activateSession();
	app.currentState.page = 1;
	app.currentState.total = 1;
	app.currentState.items = [account(1)];
	const allButton = getElement("refreshAllCredits");
	allButton.focus = function focus() {
		this.focused = true;
		document.activeElement = this;
	};
	document.activeElement = allButton;

	const refresh = app.refreshAllCredits();
	document.activeElement = document.body;
	await refresh;

	assert.equal(allButton.focused, true);
	const toast = getElement("toastContainer").children.at(-1);
	assert.equal(toast.attributes.role, "presentation");
	assert.equal(toast.attributes["aria-hidden"], "true");
	assert.match(getElement("creditsRefreshAnnouncement").textContent, /Credit 刷新完成/);
});

test("compiled credit styles and cache version cover dynamic states", () => {
	const css = fs.readFileSync(path.join(__dirname, "app.css"), "utf8");
	for (const selector of [
		".credit-refresh-button",
		".usage-credit-cell",
		".usage-credit-detail",
		".usage-credit-stale",
	]) {
		assert.match(css, new RegExp(selector.replace(".", "\\.")));
	}
	const html = fs.readFileSync(path.join(__dirname, "../templates/index.html"), "utf8");
	assert.match(html, /app\.css\?v=20260722/);
});

test("a failed credit refresh keeps the previous snapshot and marks it failed", async () => {
	const fetch = async () =>
		new Response(JSON.stringify({ error: "upstream unavailable" }), {
			status: 503,
			headers: { "Content-Type": "application/json" },
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{ ...account(1), usage_based_credits: usageCredits({ remaining: 4992 }) },
	];

	await app.refreshCreditsForAccount(1);
	const view = app.getUsageCreditsView(app.currentState.items[0]);
	assert.match(view.primary, /剩余 4,992/);
	assert.match(view.detail, /刷新失败/);
});

test("a failed first credit refresh is not rendered as zero credits", async () => {
	const fetch = async () => new Response(null, { status: 503 });
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{
			...account(1),
			usage_based_credits: {
				state: "unknown",
				consumed: 0,
				budget: 0,
				remaining: 0,
			},
		},
	];

	await app.refreshCreditsForAccount(1);
	const view = app.getUsageCreditsView(app.currentState.items[0]);
	assert.equal(view.primary, "Credit 刷新失败");
});

test("a successful account reload clears a local credit refresh error", async () => {
	const fetch = async (_url, options) => {
		if (options.method === "POST") {
			return new Response(JSON.stringify({ error: "upstream unavailable" }), {
				status: 503,
				headers: { "Content-Type": "application/json" },
			});
		}
		return new Response(
			JSON.stringify({
				items: [
					{
						...account(1),
						usage_based_credits: usageCredits({ remaining: 4980 }),
					},
				],
				total: 1,
			}),
			{ headers: { "Content-Type": "application/json" } },
		);
	};
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{ ...account(1), usage_based_credits: usageCredits({ remaining: 4992 }) },
	];

	await app.refreshCreditsForAccount(1);
	assert.match(app.getUsageCreditsView(app.currentState.items[0]).detail, /刷新失败/);
	await app.loadAccounts();

	const view = app.getUsageCreditsView(app.currentState.items[0]);
	assert.equal(app.currentState.items[0].usage_based_credits.remaining, 4980);
	assert.doesNotMatch(view.detail, /刷新失败/);
});

test("an older account-list response cannot overwrite a newer credit refresh", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({
				url,
				method: options.method || "GET",
				body: options.body,
				resolve,
				reject,
			});
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{ ...account(1), usage_based_credits: usageCredits({ remaining: 4992 }) },
	];

	const load = app.loadAccounts();
	const refresh = app.refreshCreditsForAccount(1);
	assert.equal(requests.length, 2);
	assert.equal(requests[0].method, "GET");
	assert.equal(requests[1].method, "POST");

	requests[1].resolve(
		new Response(
			JSON.stringify({
				items: [
					{
						id: 1,
						usage_based_credits: usageCredits({
							remaining: 4975,
							updated_at: "2026-07-22T00:01:00Z",
						}),
					},
				],
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await refresh;

	requests[0].resolve(
		new Response(
			JSON.stringify({
				items: [
					{
						...account(1),
						usage_based_credits: usageCredits({
							remaining: 4992,
							updated_at: "2026-07-22T00:00:00Z",
						}),
					},
				],
				total: 1,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await load;

	assert.equal(app.currentState.items[0].usage_based_credits.remaining, 4975);
});

test("an older credit refresh response cannot overwrite a newer account snapshot", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({ url, method: options.method || "GET", resolve, reject });
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [
		{ ...account(1), usage_based_credits: usageCredits({ remaining: 4992 }) },
	];

	const load = app.loadAccounts();
	const refresh = app.refreshCreditsForAccount(1);
	requests[0].resolve(
		new Response(
			JSON.stringify({
				items: [
					{
						...account(1),
						usage_based_credits: usageCredits({
							remaining: 4970,
							updated_at: "2026-07-22T00:02:00Z",
						}),
					},
				],
				total: 1,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await load;

	requests[1].resolve(
		new Response(
			JSON.stringify({
				items: [
					{
						id: 1,
						usage_based_credits: usageCredits({
							remaining: 4975,
							updated_at: "2026-07-22T00:01:00Z",
						}),
					},
				],
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await refresh;

	assert.equal(app.currentState.items[0].usage_based_credits.remaining, 4970);
});

test("all-account credit refresh caches results for accounts on other pages", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({
				url,
				method: options.method || "GET",
				body: options.body,
				resolve,
				reject,
			});
		});
	const { app } = createHarness(fetch);
	app.activateSession();
	app.currentState.page = 1;
	app.currentState.size = 1;
	app.currentState.total = 2;
	app.currentState.items = [account(1)];

	const all = app.refreshAllCredits();
	assert.equal(requests.length, 1);
	assert.equal(requests[0].method, "GET");
	requests[0].resolve(
		new Response(
			JSON.stringify({
				items: [account(1), account(2)],
				total: 2,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await new Promise((resolve) => setImmediate(resolve));
	assert.equal(requests.length, 2);
	assert.equal(requests[1].method, "POST");
	assert.deepEqual(JSON.parse(requests[1].body), { ids: [1, 2] });
	requests[1].resolve(
		new Response(
			JSON.stringify({
				items: [
					{ id: 1, usage_based_credits: usageCredits({ remaining: 4990 }) },
					{ id: 2, usage_based_credits: usageCredits({ remaining: 4980 }) },
				],
				requested: 2,
				refreshed: 2,
				skipped: 0,
				failed: 0,
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await all;
	assert.equal(app.currentState.items[0].usage_based_credits.remaining, 4990);

	app.currentState.page = 2;
	const page = app.loadAccounts();
	assert.equal(requests.length, 3);
	requests[2].resolve(
		new Response(
			JSON.stringify({ items: [account(2)], total: 2 }),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await page;
	assert.equal(app.currentState.items[0].id, 2);
	assert.equal(
		app.currentState.items[0].usage_based_credits.remaining,
		4980,
	);
});

test("a late credit refresh completion does not redraw a new session", async () => {
	const requests = [];
	const fetch = (url, options) =>
		new Promise((resolve, reject) => {
			requests.push({ url, method: options.method || "GET", resolve, reject });
		});
	const { app, getElement } = createHarness(fetch);
	app.activateSession();
	app.currentState.items = [account(1)];
	const refresh = app.refreshCreditsForAccount(1);
	assert.equal(requests[0].method, "POST");

	const logout = app.logout();
	const logoutRequest = requests.find((request) => request.method === "DELETE");
	assert.ok(logoutRequest, "logout request did not start");
	logoutRequest.resolve(new Response(null, { status: 204 }));
	await logout;

	app.activateSession();
	app.currentState.items = [account(2)];
	getElement("accountList").innerHTML = "new-session-sentinel";
	requests[0].resolve(
		new Response(
			JSON.stringify({
				items: [{ id: 1, usage_based_credits: usageCredits() }],
			}),
			{ headers: { "Content-Type": "application/json" } },
		),
	);
	await refresh;
	assert.equal(getElement("accountList").innerHTML, "new-session-sentinel");
});
