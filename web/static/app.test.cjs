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
		appendChild() {},
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
		`${source}\nglobalThis.__appTest = { createAdminSession, loadAccounts, readResponseError, submitAPIKey, startZencoderOAuth, completeZencoderOAuth, resetOAuthFlow, deleteAccount, batchDelete, currentState, logout, getOAuthFlow: () => oauthFlow, activateSession: () => { appSessionActive = true; } };`,
		context,
	);
	return { app: context.__appTest, elements, getElement };
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
