const API_BASE = "/api";
const REFRESH_INTERVAL = 3000;
const REQUEST_TIMEOUT_MS = 15 * 1000;
const OAUTH_COMPLETE_TIMEOUT_MS = 45 * 1000;
const OAUTH_TIMEOUT_MINUTES = 9;
const OAUTH_TIMEOUT_MS = OAUTH_TIMEOUT_MINUTES * 60 * 1000;
let autoRefreshTimer = null;
let accountsRequestController = null;
let accountsRequestSequence = 0;
let oauthFlow = null;
let nextOAuthFlowID = 0;
let appSessionActive = false;
let refreshGeneration = 0;
let apiKeyEditingAccount = null;
let adminLoginErrorMessage = "密码错误，请重试";
let adminLogoutInProgress = false;
let lastAccountsErrorAnnouncement = "";
let activeOAuthOperation = null;
let activeDeleteOperation = null;
let deleteOperationInFlight = false;

// The password is used only for the session exchange and is never retained.
// The CSRF token is intentionally memory-only; the server session is HttpOnly.
let adminCSRFToken = null;

// State
let currentState = {
	page: 1,
	size: 10,
	total: 0,
	selectedIds: new Set(),
	items: [],
};
const pendingAdminControllers = new Set();

function resetAdminUIState() {
	currentState.page = 1;
	currentState.total = 0;
	currentState.items = [];
	currentState.selectedIds.clear();
	if (!document.getElementById("mainApp")) return;
	renderAccounts([]);
	updatePaginationUI();
	updateBatchUI();
	updateStatsUI({ total_accounts: 0, today_usage: 0, total_usage: 0 });
	setAPIKeyStatus();
	lastAccountsErrorAnnouncement = "";
	const announcement = document.getElementById("accountsRefreshAnnouncement");
	if (announcement) announcement.textContent = "";
}

function deactivateAdminSession() {
	appSessionActive = false;
	refreshGeneration += 1;
	adminCSRFToken = null;
	cancelAccountsRequest();
	abortPendingAdminOperations();
	activeOAuthOperation = null;
	updateOAuthActionDisabled();
	activeDeleteOperation = null;
	deleteOperationInFlight = false;
	setDeleteActionsDisabled(false);
	resetOAuthFlow();
	clearAPIKeyInput();
	setAPIKeyEditMode(null);
	setAPIKeySubmitLoading(false);
	if (autoRefreshTimer) {
		clearTimeout(autoRefreshTimer);
		autoRefreshTimer = null;
	}
	resetAdminUIState();
}

function beginAdminOperation() {
	const controller = new AbortController();
	pendingAdminControllers.add(controller);
	return { controller, generation: refreshGeneration };
}

function finishAdminOperation(operation) {
	pendingAdminControllers.delete(operation.controller);
}

function updateOAuthActionDisabled() {
	const isDisabled = Boolean(activeOAuthOperation);
	const startButton = document.getElementById("oauthBtn");
	const completeButton = document.getElementById("oauthCompleteBtn");
	if (startButton) startButton.disabled = isDisabled;
	if (completeButton) completeButton.disabled = isDisabled;
}

function beginOAuthOperation() {
	if (activeOAuthOperation) return null;
	const operation = beginAdminOperation();
	activeOAuthOperation = operation;
	updateOAuthActionDisabled();
	return operation;
}

function finishOAuthOperation(operation) {
	finishAdminOperation(operation);
	if (activeOAuthOperation !== operation) return;
	activeOAuthOperation = null;
	updateOAuthActionDisabled();
}

function setDeleteActionsDisabled(isDisabled) {
	const buttons = document.querySelectorAll?.(
		'[data-action="delete-account"], [data-action="batch-delete"]',
	);
	if (!buttons) return;
	for (const button of buttons) {
		button.disabled = isDisabled;
		if (isDisabled) {
			button.setAttribute("aria-disabled", "true");
		} else {
			button.removeAttribute("aria-disabled");
		}
	}
}

function beginDeleteOperation() {
	if (activeDeleteOperation) return null;
	const operation = beginAdminOperation();
	activeDeleteOperation = operation;
	deleteOperationInFlight = true;
	setDeleteActionsDisabled(true);
	return operation;
}

function finishDeleteOperation(operation) {
	finishAdminOperation(operation);
	if (activeDeleteOperation !== operation) return;
	activeDeleteOperation = null;
	deleteOperationInFlight = false;
	setDeleteActionsDisabled(false);
}

function abortPendingAdminOperations() {
	for (const controller of pendingAdminControllers) controller.abort();
	pendingAdminControllers.clear();
}

function createTimeoutSignal(timeoutMs) {
	if (typeof AbortSignal.timeout === "function") {
		return AbortSignal.timeout(timeoutMs);
	}
	const controller = new AbortController();
	const timer = setTimeout(() => controller.abort(), timeoutMs);
	timer.unref?.();
	return controller.signal;
}

function combineAbortSignals(signals) {
	if (typeof AbortSignal.any === "function") return AbortSignal.any(signals);
	const controller = new AbortController();
	const cleanup = () => {
		for (const signal of signals) {
			signal.removeEventListener("abort", abort);
		}
	};
	const abort = () => {
		if (controller.signal.aborted) return;
		cleanup();
		controller.abort();
	};
	for (const signal of signals) {
		if (signal.aborted) {
			abort();
			break;
		}
		signal.addEventListener("abort", abort, { once: true });
	}
	return controller.signal;
}

async function fetchWithTimeout(path, options = {}, timeoutMs = REQUEST_TIMEOUT_MS) {
	const externalSignal = options.signal;
	const timeoutSignal = createTimeoutSignal(timeoutMs);
	const signal = externalSignal
		? combineAbortSignals([externalSignal, timeoutSignal])
		: timeoutSignal;

	try {
		return await fetch(path, { ...options, signal });
	} catch (error) {
		if (timeoutSignal.aborted && !externalSignal?.aborted) {
			const timeoutError = new Error("请求超时，请稍后重试");
			timeoutError.name = "TimeoutError";
			throw timeoutError;
		}
		throw error;
	}
}

function requestErrorMessage(error, fallback) {
	if (error?.name === "TimeoutError" || error?.name === "AbortError") {
		return "请求超时，请稍后重试";
	}
	return error?.message || fallback;
}

async function createAdminSession(password) {
	adminLoginErrorMessage = "密码错误，请重试";
	try {
		const response = await fetchWithTimeout(`${API_BASE}/admin/session`, {
			method: "POST",
			credentials: "same-origin",
			cache: "no-store",
			referrerPolicy: "no-referrer",
			headers: {
				Accept: "application/json",
				Authorization: `Bearer ${password}`,
			},
		});
		if (!response.ok) {
			adminLoginErrorMessage =
				response.status === 401
					? "密码错误，请重试"
					: response.status === 429
						? "尝试次数过多，请稍后重试"
						: `登录失败（HTTP ${response.status}）`;
			return false;
		}
		let result;
		try {
			result = await response.json();
		} catch (error) {
			if (error.name === "TimeoutError" || error.name === "AbortError") {
				throw error;
			}
			adminCSRFToken = null;
			adminLoginErrorMessage = "服务器返回了无效的登录会话，请稍后重试";
			return false;
		}
		if (!result || typeof result.csrfToken !== "string") {
			adminCSRFToken = null;
			adminLoginErrorMessage = "服务器返回了无效的登录会话，请稍后重试";
			return false;
		}
		adminCSRFToken =
			typeof result.csrfToken === "string" ? result.csrfToken : "";
		return true;
	} catch (error) {
		adminCSRFToken = null;
		adminLoginErrorMessage =
			error.name === "TimeoutError" || error.name === "AbortError"
				? requestErrorMessage(error, "请求超时，请稍后重试")
				: "无法连接服务器，请稍后重试";
		return false;
	}
}

function showAdminLogin() {
	const modal = document.getElementById("adminPasswordModal");
	const mainApp = document.getElementById("mainApp");
	modal.classList.remove("hidden");
	modal.setAttribute("aria-hidden", "false");
	mainApp.classList.add("hidden");
	mainApp.setAttribute("aria-hidden", "true");
	mainApp.inert = true;
	setAdminPasswordVisibility(false);
	document.getElementById("adminPassword").focus();
}

function hideAdminLogin() {
	const modal = document.getElementById("adminPasswordModal");
	const mainApp = document.getElementById("mainApp");
	modal.classList.add("hidden");
	modal.setAttribute("aria-hidden", "true");
	mainApp.classList.remove("hidden");
	mainApp.classList.add("flex");
	mainApp.setAttribute("aria-hidden", "false");
	mainApp.inert = false;
	document.getElementById("mainContent")?.focus({ preventScroll: true });
}

function setAdminLoginLoading(isLoading, label = "验证中...") {
	const modal = document.getElementById("adminPasswordModal");
	const input = document.getElementById("adminPassword");
	const visibility = document.getElementById("adminPasswordVisibility");
	const button = document.getElementById("adminLoginBtn");
	const text = document.getElementById("adminBtnText");
	const loading = document.getElementById("adminBtnLoading");
	if (!modal || !input || !visibility || !button || !text || !loading) return;
	if (isLoading) {
		modal.setAttribute("aria-busy", "true");
		modal.focus({ preventScroll: true });
	} else {
		modal.removeAttribute("aria-busy");
	}
	input.disabled = isLoading;
	visibility.disabled = isLoading;
	button.disabled = isLoading;
	text.textContent = isLoading ? label : "验证";
	loading.classList.toggle("hidden", !isLoading);
}

async function handleAdminLogin(password) {
	if (adminLogoutInProgress) return false;
	const isValid = await createAdminSession(password);

	if (isValid) {
		appSessionActive = true;
		hideAdminLogin();
		document.getElementById("passwordError").classList.add("hidden");

		// 开始加载数据
		initializeApp();
		return true;
	} else {
		const error = document.getElementById("passwordError");
		error.textContent = adminLoginErrorMessage;
		error.classList.remove("hidden");
		return false;
	}
}

async function logout() {
	if (!appSessionActive || adminLogoutInProgress) return;
	adminLogoutInProgress = true;
	deactivateAdminSession();
	showAdminLogin();
	setAdminLoginLoading(true, "正在退出...");
	let logoutError = null;
	try {
		const response = await fetchWithTimeout(`${API_BASE}/admin/session`, {
			method: "DELETE",
			credentials: "same-origin",
			cache: "no-store",
			referrerPolicy: "no-referrer",
		});
		if (!response.ok) {
			throw new Error(`服务器退出失败（HTTP ${response.status}）`);
		}
	} catch (error) {
		logoutError = requestErrorMessage(error, "服务器退出失败");
		console.warn("Admin session logout failed", error);
	}
	adminLogoutInProgress = false;
	setAdminLoginLoading(false);
	if (logoutError) {
		adminLoginErrorMessage = `未能确认服务端退出：${logoutError}`;
		const error = document.getElementById("passwordError");
		error.textContent = adminLoginErrorMessage;
		error.classList.remove("hidden");
	}
	document.getElementById("adminPassword").focus();
}

async function initAdminAuth() {
	// A reload deliberately requires a fresh exchange; no credential is stored.
	adminCSRFToken = null;
	showAdminLogin();
}

function setAdminPasswordVisibility(isVisible) {
	const input = document.getElementById("adminPassword");
	const eyeIcon = document.getElementById("adminEyeIcon");
	const button = document.getElementById("adminPasswordVisibility");
	if (!input || !eyeIcon || !button) return;

	if (isVisible) {
		input.type = "text";
		button.setAttribute("aria-label", "隐藏管理密码");
		button.setAttribute("aria-pressed", "true");
		eyeIcon.innerHTML =
			'<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13.875 18.825A10.05 10.05 0 0112 19c-4.478 0-8.268-2.943-9.543-7a9.97 9.97 0 011.563-3.029m5.858.908a3 3 0 114.243 4.243M9.878 9.878l4.242 4.242M9.88 9.88l-3.29-3.29m7.532 7.532l3.29 3.29M3 3l3.59 3.59m0 0A9.953 9.953 0 0112 5c4.478 0 8.268 2.943 9.543 7a10.025 10.025 0 01-4.132 5.411m0 0L21 21" />';
	} else {
		input.type = "password";
		button.setAttribute("aria-label", "显示管理密码");
		button.setAttribute("aria-pressed", "false");
		eyeIcon.innerHTML =
			'<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" /><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z" />';
	}
}

function toggleAdminPasswordVisibility() {
	const input = document.getElementById("adminPassword");
	setAdminPasswordVisibility(input?.type !== "text");
}

function trapAdminDialogFocus(event) {
	if (event.key !== "Tab") return;
	const modal = document.getElementById("adminPasswordModal");
	const focusable = Array.from(
		modal.querySelectorAll(
			"button:not([disabled]), input:not([disabled]), [href], [tabindex]:not([tabindex='-1'])",
		),
	).filter((element) => !element.hidden && element.offsetParent !== null);
	if (focusable.length === 0) {
		event.preventDefault();
		modal.focus();
		return;
	}
	if (focusable.length === 1) {
		event.preventDefault();
		focusable[0].focus();
		return;
	}
	const first = focusable[0];
	const last = focusable[focusable.length - 1];
	if (event.shiftKey && document.activeElement === first) {
		event.preventDefault();
		last.focus();
	} else if (!event.shiftKey && document.activeElement === last) {
		event.preventDefault();
		first.focus();
	}
}

// Admin Password Form Handler
function bindAdminPasswordForm() {
	const adminForm = document.getElementById("adminPasswordForm");
	const adminModal = document.getElementById("adminPasswordModal");
	if (adminModal && adminModal.dataset.focusBound !== "true") {
		adminModal.dataset.focusBound = "true";
		adminModal.addEventListener("keydown", trapAdminDialogFocus);
	}
	if (adminForm && adminForm.dataset.bound !== "true") {
		adminForm.dataset.bound = "true";

		adminForm.addEventListener("submit", async (e) => {
			e.preventDefault();

			const password = document
				.getElementById("adminPassword")
				.value.trim();

			if (!password) {
				document.getElementById("passwordError").textContent =
					"请输入管理密码";
				document
					.getElementById("passwordError")
					.classList.remove("hidden");
				document.getElementById("adminPassword").focus();
				return;
			}

			setAdminLoginLoading(true);

			const success = await handleAdminLogin(password);

			setAdminLoginLoading(false);

			if (success) {
				document.getElementById("adminPassword").value = "";
			} else {
				document.getElementById("adminPassword").focus();
			}
		});
	}
}

if (document.readyState === "loading") {
	document.addEventListener("DOMContentLoaded", () => {
		bindAdminPasswordForm();
		bindUIControls();
	});
} else {
	bindAdminPasswordForm();
	bindUIControls();
}

function getAuthHeaders() {
	const headers = {};
	if (adminCSRFToken) {
		headers["X-CSRF-Token"] = adminCSRFToken;
	}
	return headers;
}

function isCurrentSession(generation) {
	return appSessionActive && generation === refreshGeneration;
}

async function adminFetch(
	path,
	options = {},
	timeoutMs = REQUEST_TIMEOUT_MS,
) {
	if (!appSessionActive) {
		const error = new DOMException("管理员会话已失效", "AbortError");
		throw error;
	}
	const headers = new Headers(options.headers || {});
	for (const [name, value] of Object.entries(getAuthHeaders())) {
		headers.set(name, value);
	}
	const response = await fetchWithTimeout(path, {
		...options,
		headers,
		credentials: "same-origin",
		cache: "no-store",
		referrerPolicy: "no-referrer",
	}, timeoutMs);
	if (response.status === 401 || response.status === 403) {
		deactivateAdminSession();
		if (document.getElementById("adminPasswordModal").classList.contains("hidden")) {
			showAdminLogin();
		}
	}
	return response;
}

function setAPIKeyStatus(message = "", type = "info") {
	const status = document.getElementById("apiKeyStatus");
	if (!status) return;
	const isError = Boolean(message) && type === "error";
	status.textContent = message;
	status.classList.toggle("hidden", !message);
	status.classList.toggle("text-red-600", isError);
	status.classList.toggle("dark:text-red-400", isError);
	status.classList.toggle("text-green-600", type === "success");
	status.classList.toggle("dark:text-green-400", type === "success");
	status.setAttribute("role", isError ? "alert" : "status");
	status.setAttribute("aria-live", isError ? "assertive" : "polite");
	document
		.getElementById("apiKeyValue")
		?.setAttribute("aria-invalid", isError ? "true" : "false");
}

function setAPIKeySubmitLoading(isLoading) {
	const button = document.getElementById("apiKeySubmit");
	const text = document.getElementById("apiKeySubmitText");
	const loading = document.getElementById("apiKeySubmitLoading");
	if (!button || !text || !loading) return;
	button.disabled = isLoading;
	text.textContent = isLoading
		? "保存中..."
		: apiKeyEditingAccount
			? "轮换 API Key"
			: "保存 API Key";
	loading.classList.toggle("hidden", !isLoading);
}

async function submitAPIKey(event) {
	event.preventDefault();
	if (!appSessionActive) return;
	const nameInput = document.getElementById("apiKeyName");
	const keyInput = document.getElementById("apiKeyValue");
	const rawAPIKey = keyInput.value;
	let apiKey = rawAPIKey.trim();
	const name = nameInput.value.trim();
	setAPIKeyStatus();
	if (!apiKey) {
		setAPIKeyStatus("请输入 API Key", "error");
		keyInput.focus();
		return;
	}
	const encoder = new TextEncoder();
	const rawAPIKeyBytes = encoder.encode(rawAPIKey).length;
	const apiKeyBytes = encoder.encode(apiKey).length;
	if (apiKeyBytes < 16 || rawAPIKeyBytes > 4096) {
		setAPIKeyStatus("API Key 必须为 16 至 4096 字节", "error");
		keyInput.focus();
		return;
	}
	// Clear the DOM input before any network await; the key is never rendered back.
	keyInput.value = "";

	setAPIKeySubmitLoading(true);
	const operation = beginAdminOperation();
	try {
		const editingID = apiKeyEditingAccount?.id;
		const endpoint = editingID
			? `${API_BASE}/accounts/${editingID}/api-key`
			: `${API_BASE}/accounts/api-key`;
		const response = await adminFetch(endpoint, {
			method: editingID ? "PUT" : "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ name, api_key: apiKey }),
			signal: operation.controller.signal,
		});
		if (!isCurrentSession(operation.generation)) return;
		if (!response.ok) {
			// Do not render an arbitrary server error: it must never echo the key.
			throw new Error(`API Key 保存失败（HTTP ${response.status}）`);
		}
		setAPIKeyStatus(editingID ? "API Key 已轮换" : "API Key 已保存", "success");
		setAPIKeyEditMode(null);
		await loadAccounts();
	} catch (error) {
		if (
			!isCurrentSession(operation.generation) ||
			operation.controller.signal.aborted
		) {
			return;
		}
		setAPIKeyStatus(requestErrorMessage(error, "API Key 保存失败"), "error");
	} finally {
		apiKey = "";
		finishAdminOperation(operation);
		if (isCurrentSession(operation.generation)) {
			setAPIKeySubmitLoading(false);
		}
	}
}

function clearAPIKeyInput() {
	const input = document.getElementById("apiKeyValue");
	if (input) input.value = "";
}

function setAPIKeyEditMode(account) {
	apiKeyEditingAccount = account || null;
	const title = document.getElementById("apiKeyFormTitle");
	const helper = document.getElementById("apiKeyFormHelper");
	const name = document.getElementById("apiKeyName");
	const key = document.getElementById("apiKeyValue");
	const submitText = document.getElementById("apiKeySubmitText");
	const cancel = document.getElementById("apiKeyCancel");
	if (apiKeyEditingAccount) {
		title.textContent = "轮换 API Key";
		helper.textContent = `账号 ID ${apiKeyEditingAccount.id} 将立即改用新密钥。`;
		name.value = getOAuthAccountLabel(apiKeyEditingAccount);
		submitText.textContent = "轮换 API Key";
		cancel.classList.remove("hidden");
		key.value = "";
		key.focus();
	} else {
		title.textContent = "添加 API Key 账号";
		helper.textContent = "API key 只在提交请求中使用，提交后会立即从输入框清除。";
		name.value = "";
		submitText.textContent = "保存 API Key";
		cancel.classList.add("hidden");
		key.value = "";
	}
}

function beginAPIKeyRotation(id) {
	if (!appSessionActive) return;
	const account = currentState.items.find(
		(item) => Number(item.id) === id && item.credential_type === "api_key",
	);
	if (!account) {
		showToast("该 API Key 账号已不在当前页面，请刷新后重试", "error");
		return;
	}
	setAPIKeyStatus();
	setAPIKeyEditMode(account);
	document.getElementById("apiKeyForm").scrollIntoView({
		behavior: window.matchMedia?.("(prefers-reduced-motion: reduce)")?.matches
			? "auto"
			: "smooth",
		block: "center",
	});
}

function bindUIControls() {
	const bindOnce = (element, event, handler) => {
		if (!element || element.dataset.bound === "true") return;
		element.dataset.bound = "true";
		element.addEventListener(event, handler);
	};

	bindOnce(
		document.getElementById("adminPasswordVisibility"),
		"click",
		toggleAdminPasswordVisibility,
	);
	bindOnce(document.getElementById("logoutButton"), "click", logout);
	bindOnce(document.getElementById("apiKeyForm"), "submit", submitAPIKey);
	bindOnce(document.getElementById("apiKeyCancel"), "click", () => {
		setAPIKeyStatus();
		setAPIKeyEditMode(null);
	});
	bindOnce(document.getElementById("selectAll"), "change", toggleSelectAll);
	bindOnce(document.getElementById("prevPage"), "click", () => changePage(-1));
	bindOnce(document.getElementById("nextPage"), "click", () => changePage(1));

	for (const container of [
		document.getElementById("accountList"),
		document.getElementById("mobileAccountList"),
		document.getElementById("batchButtonsContainer"),
	]) {
		if (!container || container.dataset.bound === "true") continue;
		container.dataset.bound = "true";
		container.addEventListener("change", (event) => {
			const input = event.target.closest('[data-action="toggle-select"]');
			if (input) toggleSelect(Number(input.dataset.accountId));
		});
		container.addEventListener("click", (event) => {
			const action = event.target.closest("[data-action]");
			if (!action) return;
			if (action.dataset.action === "delete-account") {
				deleteAccount(Number(action.dataset.accountId));
			} else if (action.dataset.action === "rotate-api-key") {
				beginAPIKeyRotation(Number(action.dataset.accountId));
			} else if (action.dataset.action === "batch-delete") {
				batchDelete(action.dataset.deleteAll === "true");
			}
		});
	}
}

// --- Theme Management ---
function initTheme() {
	const isDark =
		localStorage.theme === "dark" ||
		(!("theme" in localStorage) &&
			window.matchMedia("(prefers-color-scheme: dark)").matches);

	if (isDark) {
		document.documentElement.classList.add("dark");
	} else {
		document.documentElement.classList.remove("dark");
	}
	updateThemeIcons(isDark);
}

function toggleTheme() {
	const isDark = document.documentElement.classList.toggle("dark");
	localStorage.theme = isDark ? "dark" : "light";
	updateThemeIcons(isDark);
}

function updateThemeIcons(isDark) {
	const sun = document.getElementById("sunIcon");
	const moon = document.getElementById("moonIcon");
	const toggle = document.getElementById("themeToggle");
	toggle.setAttribute(
		"aria-label",
		isDark ? "切换到浅色主题" : "切换到深色主题",
	);
	if (isDark) {
		sun.classList.remove("hidden");
		moon.classList.add("hidden");
	} else {
		sun.classList.add("hidden");
		moon.classList.remove("hidden");
	}
}

document.getElementById("themeToggle").addEventListener("click", toggleTheme);

function escapeHtml(value) {
	return String(value ?? "").replace(
		/[&<>'"]/g,
		(character) =>
			({
				"&": "&amp;",
				"<": "&lt;",
				">": "&gt;",
				"'": "&#39;",
				'"': "&quot;",
			})[character],
	);
}

function getOAuthAccountLabel(account) {
	const oauthEmail =
		typeof account.oauth_email === "string" && account.oauth_email.trim()
			? account.oauth_email.trim()
			: typeof account.email === "string"
				? account.email.trim()
				: "";
	return oauthEmail || "Zencoder 账号";
}

function getCredentialTypeLabel(account) {
	return account?.credential_type === "api_key" ? "API Key" : "OAuth";
}

function formatLastUsed(dateStr) {
	if (!dateStr || dateStr.startsWith("0001")) return "从未";

	const date = new Date(dateStr);
	const now = new Date();
	const diff = now - date;

	// 转换为秒、分钟、小时、天
	const seconds = Math.floor(diff / 1000);
	const minutes = Math.floor(seconds / 60);
	const hours = Math.floor(minutes / 60);
	const days = Math.floor(hours / 24);

	if (days > 0) {
		return `${days}天前`;
	} else if (hours > 0) {
		return `${hours}小时前`;
	} else if (minutes > 0) {
		return `${minutes}分钟前`;
	} else if (seconds > 0) {
		return `${seconds}秒前`;
	} else {
		return "刚刚";
	}
}

function cancelAccountsRequest() {
	accountsRequestSequence += 1;
	if (accountsRequestController) {
		accountsRequestController.abort();
		accountsRequestController = null;
	}
}

function setAccountsRefreshStatus(message, type = "success") {
	const label = document.getElementById("accountsRefreshStatus");
	const announcement = document.getElementById("accountsRefreshAnnouncement");
	const dot = document.querySelector("[data-accounts-status-dot]");
	if (!label || !dot) return;
	label.textContent = message;
	if (announcement) {
		if (type === "error" && lastAccountsErrorAnnouncement !== message) {
			announcement.textContent = message;
			lastAccountsErrorAnnouncement = message;
		} else if (type === "success") {
			announcement.textContent = "";
			lastAccountsErrorAnnouncement = "";
		}
	}
	const container = dot.parentElement;
	container.classList.toggle("text-gray-400", false);
	container.classList.toggle("text-gray-500", type !== "error");
	container.classList.toggle("text-red-600", type === "error");
	container.classList.toggle("dark:text-red-400", type === "error");
	dot.classList.toggle("bg-green-500", type !== "error");
	dot.classList.toggle("bg-red-500", type === "error");
	dot.classList.toggle("animate-pulse", type !== "error");
	document.getElementById("tableContainer")?.setAttribute(
		"aria-busy",
		type === "loading" ? "true" : "false",
	);
}

async function loadAccounts(isAutoRefresh = false) {
	if (!appSessionActive) return;
	if (isAutoRefresh && accountsRequestController) return;
	cancelAccountsRequest();
	const requestSequence = accountsRequestSequence;
	const requestPage = currentState.page;
	const controller = new AbortController();
	accountsRequestController = controller;
	setAccountsRefreshStatus(isAutoRefresh ? "正在刷新..." : "正在加载...", "loading");
	let retryOutOfRange = false;
	try {
		const params = new URLSearchParams({
			page: requestPage,
			size: currentState.size,
		});

		const resp = await adminFetch(`${API_BASE}/accounts?${params}`, {
			signal: controller.signal,
		});
		if (
			requestSequence !== accountsRequestSequence ||
			requestPage !== currentState.page
		) {
			return;
		}
		if (!resp.ok) {
			throw new Error(await readResponseError(resp, "账号加载失败"));
		}

		const data = await resp.json();
		if (
			requestSequence !== accountsRequestSequence ||
			requestPage !== currentState.page
		) {
			return;
		}

		const items = Array.isArray(data.items) ? data.items : [];
		const total = Math.max(0, Number(data.total) || 0);
		const totalPages = Math.max(1, Math.ceil(total / currentState.size));
		if (requestPage > totalPages) {
			currentState.page = totalPages;
			retryOutOfRange = true;
		} else {
			currentState.items = items;
			currentState.total = total;

			// Retain selection if items still exist
			const newSet = new Set();
			items.forEach((item) => {
				if (currentState.selectedIds.has(item.id)) {
					newSet.add(item.id);
				}
			});
			currentState.selectedIds = newSet;

			renderAccounts(items);
			updatePaginationUI();
			updateBatchUI();

			if (data.stats) {
				updateStatsUI(data.stats);
			}
			setAccountsRefreshStatus("自动刷新中", "success");
		}
	} catch (e) {
		if (e.name === "AbortError" && requestSequence !== accountsRequestSequence) {
			return;
		}
		if (requestSequence !== accountsRequestSequence) return;
		setAccountsRefreshStatus(
			isAutoRefresh ? "刷新失败，正在重试" : "加载失败，请重试",
			"error",
		);
		if (!isAutoRefresh) {
			console.error("Failed to load accounts", e);
			showToast(requestErrorMessage(e, "账号加载失败"), "error");
		}
	} finally {
		if (requestSequence === accountsRequestSequence) {
			accountsRequestController = null;
		}
	}
	if (retryOutOfRange) await loadAccounts(false);
}

function updateStatsUI(stats) {
	if (!stats) return;
	document.getElementById("stat-total-accounts").textContent =
		stats.total_accounts;
	document.getElementById("stat-today-usage").textContent = Number(
		stats.today_usage || 0,
	).toFixed(2);
	document.getElementById("stat-total-usage").textContent = Number(
		stats.total_usage || 0,
	).toFixed(2);
}

function renderAccounts(accounts) {
	const tbody = document.getElementById("accountList");
	const mobileList = document.getElementById("mobileAccountList");
	const emptyState = document.getElementById("emptyState");
	const tableContainer = document.getElementById("tableContainer");
	const paginationContainer = document.getElementById("paginationContainer");
	const selectAll = document.getElementById("selectAll");
	const activeElement = document.activeElement;
	const focusedList = activeElement?.closest?.(
		"#accountList, #mobileAccountList",
	);
	const focusedAction = activeElement?.dataset?.action;
	const focusedAccountID = activeElement?.dataset?.accountId;

	if (accounts.length === 0) {
		tbody.innerHTML = "";
		mobileList.innerHTML = "";
		tableContainer.classList.add("hidden");
		paginationContainer.classList.add("hidden");

		emptyState.classList.remove("hidden");
		emptyState.classList.add("flex");
		selectAll.checked = false;
		selectAll.disabled = true;
		if (focusedList) emptyState.focus({ preventScroll: true });
		return;
	}

	tableContainer.classList.remove("hidden");
	paginationContainer.classList.remove("hidden");

	emptyState.classList.add("hidden");
	emptyState.classList.remove("flex");

	selectAll.disabled = false;
	selectAll.checked =
		accounts.length > 0 &&
		accounts.every((a) => currentState.selectedIds.has(a.id));

	const rows = accounts.map((acc) => {
		const isSelected = currentState.selectedIds.has(acc.id);
		const lastUsedText = formatLastUsed(acc.last_used);
		const accountLabel = getOAuthAccountLabel(acc);
		const credentialTypeLabel = getCredentialTypeLabel(acc);
		const canRotateAPIKey = acc.credential_type === "api_key";
		const escapedAccountLabel = escapeHtml(accountLabel);
		const escapedLastUsedText = escapeHtml(lastUsedText);
		const accountId = Number(acc.id);
		const safeAccountId =
			Number.isSafeInteger(accountId) && accountId >= 0 ? accountId : 0;
		const todayUsage = Number(acc.daily_used || 0).toFixed(2);
		const totalUsage = Number(acc.total_used || 0).toFixed(2);
		const lastUsedTime =
			acc.last_used &&
			typeof acc.last_used === "string" &&
			!acc.last_used.startsWith("0001")
				? new Date(acc.last_used).toLocaleTimeString("zh-CN", {
						hour: "2-digit",
						minute: "2-digit",
					})
				: "";

		const desktop = `
        <tr class="hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors ${isSelected ? "bg-blue-50 dark:bg-blue-900/10" : ""}">
            <td class="px-6 py-4 whitespace-nowrap text-center">
                <input type="checkbox" data-action="toggle-select" data-account-id="${safeAccountId}" ${isSelected ? "checked" : ""} aria-label="选择账号 ${escapedAccountLabel}" class="rounded border-gray-300 dark:border-gray-600 text-primary focus:ring-primary h-4 w-4 mx-auto">
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center">
                <div class="text-sm font-medium text-gray-900 dark:text-white">${safeAccountId}</div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-left">
                <div class="flex items-center gap-2 min-w-[160px]">
                    <span class="inline-flex items-center rounded-md bg-blue-50 px-2 py-1 text-[10px] font-semibold text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-900/20 dark:text-blue-300 dark:ring-blue-400/30">
                        ${credentialTypeLabel}
                    </span>
                    <span class="max-w-[220px] truncate text-sm text-gray-700 dark:text-gray-200" title="${escapedAccountLabel}">${escapedAccountLabel}</span>
                </div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center">
                <div class="text-sm">
					<span class="${lastUsedText === "从未" ? "text-gray-500 dark:text-gray-400" : "text-gray-600 dark:text-gray-300"}">${escapedLastUsedText}</span>
					${lastUsedTime ? `<div class="text-[10px] text-gray-500 dark:text-gray-400 mt-0.5">${escapeHtml(lastUsedTime)}</div>` : ""}
                </div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center text-sm">
                <div class="text-blue-600 dark:text-blue-400">今日 ${todayUsage}</div>
                <div class="text-xs text-gray-500 dark:text-gray-400 mt-1">累计 ${totalUsage}</div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center text-sm font-medium">
				<div class="flex justify-center gap-3">
					${canRotateAPIKey ? `<button type="button" data-action="rotate-api-key" data-account-id="${safeAccountId}" class="account-action-button text-blue-600 hover:text-blue-800 focus:outline-none focus:ring-2 focus:ring-blue-500 rounded-md transition-colors" title="轮换 API Key" aria-label="轮换 API Key ${escapedAccountLabel}">
						<svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v6h6M20 20v-6h-6M5.1 15a7 7 0 0011.2 2.1L20 14M4 10l3.7-3.1A7 7 0 0118.9 9" /></svg>
					</button>` : ""}
					<button type="button" data-action="delete-account" data-account-id="${safeAccountId}" ${deleteOperationInFlight ? 'disabled aria-disabled="true"' : ""} class="account-action-button text-red-500 hover:text-red-700 focus:outline-none focus:ring-2 focus:ring-red-500 rounded-md transition-colors disabled:cursor-wait disabled:opacity-50" title="删除" aria-label="删除账号 ${escapedAccountLabel}">
                        <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                        </svg>
                    </button>
                </div>
            </td>
        </tr>`;

		const mobile = `
        <article class="px-4 py-4 transition-colors ${isSelected ? "bg-blue-50 dark:bg-blue-900/10" : "bg-surface-light dark:bg-surface-dark"}">
            <div class="flex items-start gap-3">
                <label class="account-select-control shrink-0">
                    <input type="checkbox" data-action="toggle-select" data-account-id="${safeAccountId}" ${isSelected ? "checked" : ""} aria-label="选择账号 ${escapedAccountLabel}" class="rounded border-gray-300 dark:border-gray-600 text-primary focus:ring-primary h-4 w-4">
                </label>
                <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-2">
                        <span class="inline-flex shrink-0 items-center rounded-md bg-blue-50 px-2 py-1 text-[10px] font-semibold text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-900/20 dark:text-blue-300 dark:ring-blue-400/30">${credentialTypeLabel}</span>
                        <span class="truncate text-sm font-medium text-gray-900 dark:text-white" title="${escapedAccountLabel}">${escapedAccountLabel}</span>
                    </div>
					<div class="mt-1 text-[11px] text-gray-500 dark:text-gray-400">账号 ID ${safeAccountId}</div>
                </div>
				<div class="-mr-1 flex items-center">
				${canRotateAPIKey ? `<button type="button" data-action="rotate-api-key" data-account-id="${safeAccountId}" class="account-action-button rounded-lg p-2 text-blue-600 hover:bg-blue-50 hover:text-blue-800 focus:outline-none focus:ring-2 focus:ring-blue-500 dark:hover:bg-blue-900/20 transition-colors" title="轮换 API Key" aria-label="轮换 API Key ${escapedAccountLabel}"><svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v6h6M20 20v-6h-6M5.1 15a7 7 0 0011.2 2.1L20 14M4 10l3.7-3.1A7 7 0 0118.9 9" /></svg></button>` : ""}
				<button type="button" data-action="delete-account" data-account-id="${safeAccountId}" ${deleteOperationInFlight ? 'disabled aria-disabled="true"' : ""} class="account-action-button rounded-lg p-2 text-red-500 hover:bg-red-50 hover:text-red-700 focus:outline-none focus:ring-2 focus:ring-red-500 dark:hover:bg-red-900/20 transition-colors disabled:cursor-wait disabled:opacity-50" title="删除" aria-label="删除账号 ${escapedAccountLabel}">
                    <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                    </svg>
				</button>
				</div>
            </div>
            <div class="mt-3 grid grid-cols-2 gap-3 border-t border-gray-100 pt-3 dark:border-gray-800">
                <div>
					<div class="text-[10px] font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">最后使用</div>
                    <div class="mt-1 text-xs text-gray-600 dark:text-gray-300">${escapedLastUsedText}${lastUsedTime ? ` · ${escapeHtml(lastUsedTime)}` : ""}</div>
                </div>
                <div class="text-right">
					<div class="text-[10px] font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">使用量</div>
                    <div class="mt-1 text-xs text-blue-600 dark:text-blue-400">今日 ${todayUsage}</div>
                    <div class="mt-0.5 text-[10px] text-gray-500 dark:text-gray-400">累计 ${totalUsage}</div>
                </div>
            </div>
        </article>`;

		return { desktop, mobile };
	});

	const html = rows.map((row) => row.desktop).join("");
	const mobileHtml = rows.map((row) => row.mobile).join("");

	// Optimization: Only update if content changed
	if (tbody.innerHTML !== html) {
		tbody.innerHTML = html;
	}
	if (mobileList.innerHTML !== mobileHtml) {
		mobileList.innerHTML = mobileHtml;
	}
	if (focusedList && !activeElement.isConnected && focusedAction) {
		focusedList
			.querySelector(
				`[data-action="${focusedAction}"][data-account-id="${focusedAccountID}"]`,
			)
			?.focus({ preventScroll: true });
	}
}

// --- Interactions ---

function changePage(delta) {
	const newPage = currentState.page + delta;
	if (
		newPage > 0 &&
		newPage <= Math.ceil(currentState.total / currentState.size)
	) {
		currentState.page = newPage;
		loadAccounts();
	}
}

function updatePaginationUI() {
	const totalPages = Math.ceil(currentState.total / currentState.size);
	document.getElementById("pageStart").textContent =
		currentState.total === 0
			? 0
			: (currentState.page - 1) * currentState.size + 1;
	document.getElementById("pageEnd").textContent = Math.min(
		currentState.page * currentState.size,
		currentState.total,
	);
	document.getElementById("totalItems").textContent = currentState.total;

	document.getElementById("prevPage").disabled = currentState.page <= 1;
	document.getElementById("nextPage").disabled =
		currentState.page >= totalPages;
}

function toggleSelectAll() {
	const selectAll = document.getElementById("selectAll");
	if (selectAll.checked) {
		currentState.items.forEach((item) =>
			currentState.selectedIds.add(item.id),
		);
	} else {
		currentState.selectedIds.clear();
	}
	renderAccounts(currentState.items); // re-render to show selection state
	updateBatchUI();
}

function toggleSelect(id) {
	if (currentState.selectedIds.has(id)) {
		currentState.selectedIds.delete(id);
	} else {
		currentState.selectedIds.add(id);
	}
	renderAccounts(currentState.items);
	updateBatchUI();
}

function updateBatchUI() {
	const batchActions = document.getElementById("batchActions");
	const countSpan = document.getElementById("selectedCount");
	const count = currentState.selectedIds.size;

	// 始终显示批量操作区域
	batchActions.classList.remove("hidden");
	batchActions.classList.add("flex");

	// 根据当前tab和选中状态显示不同的按钮
	const buttonsHtml = getBatchButtonsHtml(count);

	// 更新按钮区域内容
	if (count > 0) {
		countSpan.textContent = `${count} 选中`;
		countSpan.classList.remove("hidden");
	} else {
		countSpan.classList.add("hidden");
	}

	// 更新按钮容器
	const buttonsContainer = document.getElementById("batchButtonsContainer");
	if (buttonsContainer && buttonsContainer.innerHTML !== buttonsHtml) {
		buttonsContainer.innerHTML = buttonsHtml;
	}
}

function getBatchButtonsHtml(selectedCount) {
	return `
        <button type="button" data-action="batch-delete" data-delete-all="${selectedCount === 0}" ${deleteOperationInFlight ? 'disabled aria-disabled="true"' : ""} class="px-3 py-1.5 bg-red-700 text-white text-xs font-medium rounded-md hover:bg-red-800 focus:outline-none focus:ring-2 focus:ring-red-500 focus:ring-offset-2 transition-colors flex items-center gap-1 disabled:cursor-wait disabled:opacity-50">
            <svg xmlns="http://www.w3.org/2000/svg" class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
            </svg>
            ${selectedCount > 0 ? "删除选中" : "删除全部"}
        </button>
    `;
}

// 批量删除功能
async function batchDelete(deleteAll = false) {
	if (!appSessionActive || activeDeleteOperation) return;
	let confirmMsg;
	let requestData;

	if (deleteAll) {
		// 删除所有账号
		confirmMsg = `确定要删除所有账号吗？此操作不可逆转！`;
		requestData = {
			delete_all: true,
		};
	} else {
		// 删除选中的账号
		if (currentState.selectedIds.size === 0) {
			alert("请先选择要删除的账号");
			return;
		}
		confirmMsg = `确定要删除选中的 ${currentState.selectedIds.size} 个账号吗？此操作不可逆转！`;
		requestData = {
			ids: Array.from(currentState.selectedIds),
		};
	}

	if (!confirm(confirmMsg)) return;
	const operation = beginDeleteOperation();
	if (!operation) return;

	try {
		const resp = await adminFetch(`${API_BASE}/accounts/batch/delete`, {
			method: "POST",
			headers: {
				"Content-Type": "application/json",
			},
			body: JSON.stringify(requestData),
			signal: operation.controller.signal,
		});
		if (!isCurrentSession(operation.generation)) return;

		if (!resp.ok) {
			throw new Error(await readResponseError(resp, "批量删除失败"));
		}

		const result = await resp.json();
		if (!isCurrentSession(operation.generation)) return;
		alert(`成功删除 ${result.deleted_count || 0} 个账号`);

		// 清空选择并刷新页面
		currentState.selectedIds.clear();
		await loadAccounts();
	} catch (error) {
		if (
			!isCurrentSession(operation.generation) ||
			operation.controller.signal.aborted
		) {
			return;
		}
		alert(requestErrorMessage(error, "批量删除失败"));
	} finally {
		finishDeleteOperation(operation);
	}
}

// --- Zencoder OAuth ---
function setOAuthButtonState(mode = "idle") {
	const button = document.getElementById("oauthBtn");
	const text = document.getElementById("oauthBtnText");
	const icon = document.getElementById("oauthBtnIcon");
	const loading = document.getElementById("oauthBtnLoading");

	if (!button || !text || !icon || !loading) return;

	const isLoading = mode === "loading";
	text.textContent = isLoading
		? "正在生成…"
		: mode === "copied"
			? "已复制 · 再次复制"
			: "复制授权链接";
	icon.classList.toggle("hidden", isLoading);
	loading.classList.toggle("hidden", !isLoading);
}

function setOAuthStatus(message = "", type = "info") {
	const status = document.getElementById("oauthStatus");
	if (!status) return;
	status.textContent = message;
	status.classList.toggle("hidden", !message);
	status.classList.toggle("text-red-600", type === "error");
	status.classList.toggle("dark:text-red-400", type === "error");
	status.classList.toggle("text-primary", type !== "error");
	status.classList.toggle("dark:text-blue-400", type !== "error");
}

function setOAuthActiveStep(activeStep) {
	for (let step = 1; step <= 3; step += 1) {
		const item = document.getElementById(`oauthStep${step}`);
		if (!item) continue;
		const isActive = step === activeStep;
		item.classList.toggle("bg-blue-50", isActive);
		item.classList.toggle("text-blue-900", isActive);
		item.classList.toggle("dark:bg-blue-900/20", isActive);
		item.classList.toggle("dark:text-blue-100", isActive);
		item.classList.toggle("text-gray-800", !isActive);
		item.classList.toggle("dark:text-gray-100", !isActive);
	}
}

function setOAuthValidation(message = "") {
	const input = document.getElementById("oauthCallbackURL");
	const error = document.getElementById("oauthCallbackError");
	if (!input || !error) return;
	error.textContent = message;
	error.classList.toggle("hidden", !message);
	input.setAttribute("aria-invalid", message ? "true" : "false");
	input.classList.toggle("border-red-500", Boolean(message));
}

function isCurrentOAuthFlow(flow) {
	return Boolean(flow && oauthFlow && flow.id === oauthFlow.id);
}

function scheduleOAuthExpiry(flow) {
	if (!isCurrentOAuthFlow(flow)) return false;
	clearTimeout(flow.timeoutTimer);
	const remainingMs = flow.expiresAt - Date.now();
	if (remainingMs <= 0) {
		expireOAuthFlow(flow);
		return false;
	}
	flow.timeoutTimer = setTimeout(() => expireOAuthFlow(flow), remainingMs);
	return true;
}

function pauseOAuthExpiry(flow) {
	if (!isCurrentOAuthFlow(flow)) return false;
	clearTimeout(flow.timeoutTimer);
	flow.timeoutTimer = null;
	return true;
}

function resetOAuthFlow({ clearInput = true, expectedFlow = null } = {}) {
	if (expectedFlow && !isCurrentOAuthFlow(expectedFlow)) return false;
	if (oauthFlow) {
		clearTimeout(oauthFlow.timeoutTimer);
	}
	oauthFlow = null;
	setOAuthButtonState("idle");
	setOAuthCompleteLoading(false);
	setOAuthStatus();
	setOAuthValidation();
	const input = document.getElementById("oauthCallbackURL");
	if (input && clearInput) input.value = "";
	setOAuthActiveStep(1);
	return true;
}

function expireOAuthFlow(flow) {
	if (!resetOAuthFlow({ expectedFlow: flow })) return;
	showToast(`授权链接已超过 ${OAUTH_TIMEOUT_MINUTES} 分钟，请重新复制`, "error");
}

async function copyTextWithFallback(value) {
	if (navigator.clipboard && window.isSecureContext) {
		try {
			await navigator.clipboard.writeText(value);
			return;
		} catch (_) {
			// Fall through for browsers that expose the API but deny the request.
		}
	}

	const helper = document.createElement("textarea");
	helper.value = value;
	helper.setAttribute("readonly", "");
	helper.style.position = "fixed";
	helper.style.top = "0";
	helper.style.left = "-9999px";
	helper.style.opacity = "0";
	document.body.appendChild(helper);
	helper.focus();
	helper.select();
	helper.setSelectionRange(0, helper.value.length);
	let copied = false;
	try {
		copied = document.execCommand("copy");
	} finally {
		helper.remove();
	}
	if (!copied) throw new Error("浏览器拒绝访问剪贴板");
}

async function readResponseError(response, fallback) {
	try {
		const data = await response.json();
		if (typeof data.error === "string" && data.error.trim())
			return data.error.trim();
		if (
			data.error &&
			typeof data.error.message === "string" &&
			data.error.message.trim()
		)
			return data.error.message.trim();
		if (typeof data.message === "string" && data.message.trim())
			return data.message.trim();
	} catch (error) {
		if (error?.name === "TimeoutError" || error?.name === "AbortError") {
			throw error;
		}
		// The status code below is more useful than a non-JSON response body.
	}
	return `${fallback}（HTTP ${response.status}）`;
}

async function startZencoderOAuth() {
	if (!appSessionActive || activeOAuthOperation) return;
	const operation = beginOAuthOperation();
	if (!operation) return;
	setOAuthButtonState("loading");
	const initialFlow = oauthFlow;
	let flow = initialFlow;

	try {
		if (!flow) {
			const response = await adminFetch(`${API_BASE}/oauth/zencoder/start`, {
				method: "POST",
				signal: operation.controller.signal,
			});
			if (!isCurrentSession(operation.generation)) return;

			if (!response.ok) {
				throw new Error(
					await readResponseError(response, "无法启动 Zencoder 授权"),
				);
			}

			const data = await response.json();
			if (!isCurrentSession(operation.generation)) return;
			if (
				!data ||
				typeof data.authorization_url !== "string" ||
				!data.authorization_url.trim() ||
				typeof data.state !== "string" ||
				!data.state.trim()
			) {
				throw new Error("服务器未返回有效的授权信息");
			}

			const authorizationURL = new URL(data.authorization_url);
			if (!["http:", "https:"].includes(authorizationURL.protocol)) {
				throw new Error("服务器返回了不受支持的授权地址");
			}
			if (oauthFlow !== initialFlow) return;
			oauthFlow = {
				id: ++nextOAuthFlowID,
				authorizationURL: authorizationURL.href,
				state: data.state,
				expiresAt: Date.now() + OAUTH_TIMEOUT_MS,
				timeoutTimer: null,
			};
			flow = oauthFlow;
			scheduleOAuthExpiry(flow);
		}

		if (!isCurrentSession(operation.generation) || !isCurrentOAuthFlow(flow)) {
			return;
		}
		await copyTextWithFallback(flow.authorizationURL);
		if (
			!isCurrentSession(operation.generation) ||
			!isCurrentOAuthFlow(flow)
		) {
			return;
		}
		setOAuthActiveStep(1);
		setOAuthButtonState("copied");
		setOAuthStatus("授权链接已复制，请按下方步骤继续");
		showToast("授权链接已复制", "success");
	} catch (error) {
		if (
			!isCurrentSession(operation.generation) ||
			operation.controller.signal.aborted
		) {
			return;
		}
		const flowStillCurrent = flow
			? isCurrentOAuthFlow(flow)
			: oauthFlow === initialFlow;
		if (!flowStillCurrent) return;
		if (flow) resetOAuthFlow({ expectedFlow: flow });
		else resetOAuthFlow();
		showToast(
			`无法复制授权链接：${requestErrorMessage(error, "请稍后重试")}`,
			"error",
		);
	} finally {
		finishOAuthOperation(operation);
	}
}

function validateOAuthCallbackURL(rawValue) {
	if (!oauthFlow) throw new Error("授权链接已失效，请重新复制");
	const value = rawValue.trim();
	if (!value) throw new Error("请粘贴完整的 localhost 回调地址");

	let callbackURL;
	try {
		callbackURL = new URL(value);
	} catch (_) {
		throw new Error("回调地址格式无效，请从浏览器地址栏重新复制");
	}
	if (!["http:", "https:"].includes(callbackURL.protocol)) {
		throw new Error("回调地址必须以 http:// 或 https:// 开头");
	}
	if (callbackURL.username || callbackURL.password) {
		throw new Error("回调地址不能包含用户名或密码");
	}
	if (callbackURL.hostname.toLowerCase() !== "localhost") {
		throw new Error("回调地址的主机名必须是 localhost");
	}
	const expectedPath = `/oauth/zencoder/callback/${oauthFlow.state}`;
	if (callbackURL.pathname !== expectedPath) {
		throw new Error("回调地址与当前授权链接不匹配，请检查后重试");
	}
	const codes = callbackURL.searchParams.getAll("code");
	if (codes.length !== 1 || !codes[0].trim()) {
		throw new Error("回调地址中缺少有效的授权码 code");
	}
	if (callbackURL.hash) throw new Error("回调地址不能包含片段标识");
	return callbackURL.href;
}

function setOAuthCompleteLoading(isLoading) {
	const button = document.getElementById("oauthCompleteBtn");
	const text = document.getElementById("oauthCompleteBtnText");
	const loading = document.getElementById("oauthCompleteLoading");
	if (!button || !text || !loading) return;
	text.textContent = isLoading ? "正在连接…" : "提交回调地址";
	loading.classList.toggle("hidden", !isLoading);
}

async function completeZencoderOAuth(event) {
	event.preventDefault();
	if (!appSessionActive || activeOAuthOperation) return;
	const input = document.getElementById("oauthCallbackURL");
	setOAuthValidation();
	setOAuthActiveStep(3);

	let callbackURL;
	try {
		callbackURL = validateOAuthCallbackURL(input?.value || "");
	} catch (error) {
		setOAuthValidation(error.message);
		input?.focus();
		return;
	}
	const flow = oauthFlow;
	const operation = beginOAuthOperation();
	if (!operation) return;
	if (!pauseOAuthExpiry(flow)) {
		finishOAuthOperation(operation);
		return;
	}

	setOAuthCompleteLoading(true);
	try {
		const response = await adminFetch(
			`${API_BASE}/oauth/zencoder/complete`,
			{
				method: "POST",
				headers: {
					"Content-Type": "application/json",
				},
				body: JSON.stringify({ callback_url: callbackURL }),
				signal: operation.controller.signal,
			},
			OAUTH_COMPLETE_TIMEOUT_MS,
		);
		if (
			!isCurrentSession(operation.generation) ||
			!isCurrentOAuthFlow(flow)
		) {
			return;
		}
		let data = {};
		try {
			data = await response.json();
		} catch (error) {
			if (error?.name === "TimeoutError" || error?.name === "AbortError") {
				throw error;
			}
			// The fallback below includes the HTTP status without exposing the URL.
		}
		if (
			!isCurrentSession(operation.generation) ||
			!isCurrentOAuthFlow(flow)
		) {
			return;
		}
		if (!response.ok) {
			const message =
				typeof data.error === "string" && data.error.trim()
					? data.error.trim()
					: `无法完成授权（HTTP ${response.status}）`;
			if (data.reset_flow === true) {
				if (!resetOAuthFlow({ expectedFlow: flow })) return;
				showToast(message, "error");
				return;
			}
			throw new Error(message);
		}

		if (!resetOAuthFlow({ expectedFlow: flow })) return;
		const completedFlowID = flow.id;
		currentState.page = 1;
		currentState.selectedIds.clear();
		await loadAccounts();
		if (
			!isCurrentSession(operation.generation) ||
			nextOAuthFlowID !== completedFlowID
		) {
			return;
		}
		showToast(
			typeof data.message === "string" && data.message.trim()
				? data.message.trim()
				: "Zencoder 账号连接成功",
			"success",
		);
	} catch (error) {
		if (
			!isCurrentSession(operation.generation) ||
			operation.controller.signal.aborted ||
			!isCurrentOAuthFlow(flow)
		) {
			return;
		}
		if (!scheduleOAuthExpiry(flow)) return;
		setOAuthValidation(
			requestErrorMessage(error, "无法完成授权，请检查地址后重试"),
		);
		setOAuthStatus("回调提交失败，请修正后重试", "error");
		input?.focus();
	} finally {
		finishOAuthOperation(operation);
		if (
			isCurrentSession(operation.generation) &&
			isCurrentOAuthFlow(flow)
		) {
			setOAuthCompleteLoading(false);
		}
	}
}

function consumeOAuthRedirectResult() {
	const currentURL = new URL(window.location.href);
	const result = currentURL.searchParams.get("oauth");
	if (result !== "success" && result !== "error") return;

	currentURL.searchParams.delete("oauth");
	window.history.replaceState(
		{},
		document.title,
		currentURL.pathname + currentURL.search + currentURL.hash,
	);
	if (result === "success") {
		showToast("Zencoder 账号连接成功", "success");
	} else {
		showToast("Zencoder 授权失败，请重试", "error");
	}
}

document
	.getElementById("oauthBtn")
	.addEventListener("click", startZencoderOAuth);
document
	.getElementById("oauthCompleteForm")
	.addEventListener("submit", completeZencoderOAuth);
document.getElementById("oauthCallbackURL").addEventListener("focus", () => {
	setOAuthActiveStep(3);
});
document.getElementById("oauthCallbackURL").addEventListener("input", () => {
	setOAuthValidation();
	setOAuthActiveStep(3);
});

async function deleteAccount(id) {
	if (!appSessionActive || activeDeleteOperation) return;
	if (!confirm("确定要删除此账号吗？")) return;
	const operation = beginDeleteOperation();
	if (!operation) return;
	try {
		const response = await adminFetch(`${API_BASE}/accounts/${id}`, {
			method: "DELETE",
			signal: operation.controller.signal,
		});
		if (!isCurrentSession(operation.generation)) return;
		if (!response.ok) {
			throw new Error(await readResponseError(response, "删除失败"));
		}
		await loadAccounts();
	} catch (error) {
		if (
			!isCurrentSession(operation.generation) ||
			operation.controller.signal.aborted
		) {
			return;
		}
		alert(requestErrorMessage(error, "删除失败"));
	} finally {
		finishDeleteOperation(operation);
	}
}

// Initialization function for after admin login
function initializeApp() {
	appSessionActive = true;
	const generation = ++refreshGeneration;
	loadAccounts();

	// Auto Refresh
	if (autoRefreshTimer) {
		clearTimeout(autoRefreshTimer);
	}
	const refresh = async () => {
		await loadAccounts(true);
		if (appSessionActive && generation === refreshGeneration) {
			autoRefreshTimer = setTimeout(refresh, REFRESH_INTERVAL);
		}
	};
	autoRefreshTimer = setTimeout(refresh, REFRESH_INTERVAL);
}

// Toast提示功能
function showToast(message, type = "info") {
	// 创建toast元素
	const toast = document.createElement("div");
	const isError = type === "error";
	toast.setAttribute("role", isError ? "alert" : "status");
	toast.setAttribute("aria-live", isError ? "assertive" : "polite");
	toast.setAttribute("aria-atomic", "true");
	toast.className = `admin-toast px-6 py-3 rounded-lg shadow-lg transition-all duration-300 transform translate-x-96`;

	// 根据类型设置样式
	const styles = {
		success: "bg-green-600 text-white",
		error: "bg-red-600 text-white",
		warning: "bg-yellow-500 text-white",
		info: "bg-blue-600 text-white",
	};

	toast.className += ` ${styles[type] || styles.info}`;
	const content = document.createElement("div");
	content.className = "flex items-center gap-2";
	const text = document.createElement("span");
	text.className = "text-sm font-medium";
	text.textContent = String(message ?? "");
	content.appendChild(text);
	toast.appendChild(content);

	document.getElementById("toastContainer").appendChild(toast);

	// 动画显示
	setTimeout(() => {
		toast.classList.remove("translate-x-96");
		toast.classList.add("translate-x-0");
	}, 10);

	// 3秒后自动消失
	setTimeout(() => {
		toast.classList.remove("translate-x-0");
		toast.classList.add("translate-x-96");
		setTimeout(() => {
			toast.remove();
		}, 300);
	}, 3000);
}

// Page Initialization
window.addEventListener("load", async function () {
	bindUIControls();
	initTheme();
	await initAdminAuth();
	consumeOAuthRedirectResult();
});
