const API_BASE = "/api";
const REFRESH_INTERVAL = 3000;
const OAUTH_TIMEOUT_MS = 5 * 60 * 1000;
let autoRefreshTimer = null;
let oauthFlow = null;

// Admin Password Management
let adminPassword = null;
const ADMIN_STORAGE_KEY = "zencoder_admin_pass";

// State
let currentState = {
	page: 1,
	size: 10,
	total: 0,
	selectedIds: new Set(),
	items: [],
};

// --- Admin Password Management ---
function savePassword(password) {
	try {
		localStorage.setItem(ADMIN_STORAGE_KEY, password);
		console.log("Password saved to localStorage");
		return true;
	} catch (e) {
		console.error("Failed to save password to localStorage:", e);
		return false;
	}
}

function getSavedPassword() {
	try {
		const saved = localStorage.getItem(ADMIN_STORAGE_KEY);
		if (saved) {
			console.log("Found saved password in localStorage");
		}
		return saved;
	} catch (e) {
		console.error("Failed to get password from localStorage:", e);
		return null;
	}
}

function clearSavedPassword() {
	try {
		localStorage.removeItem(ADMIN_STORAGE_KEY);
	} catch (e) {
		console.error("Failed to clear password from localStorage:", e);
	}
}

async function verifyAdminPassword(password) {
	try {
		// 尝试调用一个需要管理密码的API来验证
		const response = await fetch(`${API_BASE}/accounts?page=1&size=1`, {
			headers: {
				"X-Admin-Password": password,
			},
		});
		return response.ok;
	} catch (e) {
		return false;
	}
}

function showAdminLogin() {
	document.getElementById("adminPasswordModal").classList.remove("hidden");
	document.getElementById("mainApp").classList.add("hidden");
	document.getElementById("adminPassword").focus();
}

function hideAdminLogin() {
	document.getElementById("adminPasswordModal").classList.add("hidden");
	document.getElementById("mainApp").classList.remove("hidden");
	document.getElementById("mainApp").classList.add("flex");
}

async function handleAdminLogin(password, remember = false) {
	console.log("Attempting login, remember:", remember);

	const isValid = await verifyAdminPassword(password);

	if (isValid) {
		adminPassword = password;

		if (remember) {
			const saved = savePassword(password);
			if (!saved) {
				console.warn("Failed to save password to localStorage");
			}
		} else {
			// 如果没有勾选记住，清除之前保存的密码
			clearSavedPassword();
		}

		hideAdminLogin();
		document.getElementById("passwordError").classList.add("hidden");

		// 开始加载数据
		initializeApp();
		return true;
	} else {
		document.getElementById("passwordError").classList.remove("hidden");
		return false;
	}
}

function logout() {
	resetOAuthFlow();
	adminPassword = null;
	clearSavedPassword();

	// 停止自动刷新
	if (autoRefreshTimer) {
		clearInterval(autoRefreshTimer);
		autoRefreshTimer = null;
	}

	// 显示登录界面
	showAdminLogin();
}

async function initAdminAuth() {
	// 检查是否有保存的密码
	const savedPassword = getSavedPassword();

	if (savedPassword) {
		console.log("Found saved password, attempting auto-login...");
		// 直接设置密码，不验证（因为验证可能由于网络问题失败）
		adminPassword = savedPassword;

		// 尝试验证密码
		try {
			const isValid = await verifyAdminPassword(savedPassword);
			if (isValid) {
				console.log("Saved password validated successfully");
				hideAdminLogin();
				initializeApp();
				return;
			} else {
				console.log("Saved password validation failed");
				// 密码无效，清除并显示登录界面
				adminPassword = null;
				clearSavedPassword();
			}
		} catch (e) {
			console.log(
				"Password validation error, keeping saved password:",
				e,
			);
			// 网络错误时仍然保留密码并尝试使用
			hideAdminLogin();
			initializeApp();
			return;
		}
	}

	// 显示登录界面
	showAdminLogin();
}

function toggleAdminPasswordVisibility() {
	const input = document.getElementById("adminPassword");
	const eyeIcon = document.getElementById("adminEyeIcon");

	if (input.type === "password") {
		input.type = "text";
		eyeIcon.innerHTML =
			'<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13.875 18.825A10.05 10.05 0 0112 19c-4.478 0-8.268-2.943-9.543-7a9.97 9.97 0 011.563-3.029m5.858.908a3 3 0 114.243 4.243M9.878 9.878l4.242 4.242M9.88 9.88l-3.29-3.29m7.532 7.532l3.29 3.29M3 3l3.59 3.59m0 0A9.953 9.953 0 0112 5c4.478 0 8.268 2.943 9.543 7a10.025 10.025 0 01-4.132 5.411m0 0L21 21" />';
	} else {
		input.type = "password";
		eyeIcon.innerHTML =
			'<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" /><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z" />';
	}
}

// Admin Password Form Handler
function bindAdminPasswordForm() {
	const adminForm = document.getElementById("adminPasswordForm");
	if (adminForm && adminForm.dataset.bound !== "true") {
		adminForm.dataset.bound = "true";
		// 检查并恢复记住密码的勾选状态
		const hasSavedPassword = getSavedPassword() !== null;
		if (hasSavedPassword) {
			const rememberCheckbox =
				document.getElementById("rememberPassword");
			if (rememberCheckbox) {
				rememberCheckbox.checked = true;
			}
		}

		adminForm.addEventListener("submit", async (e) => {
			e.preventDefault();

			const password = document
				.getElementById("adminPassword")
				.value.trim();
			const remember =
				document.getElementById("rememberPassword").checked;
			const btn = document.getElementById("adminLoginBtn");
			const btnText = document.getElementById("adminBtnText");
			const btnLoading = document.getElementById("adminBtnLoading");

			if (!password) {
				document.getElementById("passwordError").textContent =
					"请输入管理密码";
				document
					.getElementById("passwordError")
					.classList.remove("hidden");
				return;
			}

			btn.disabled = true;
			btnText.textContent = "验证中...";
			btnLoading.classList.remove("hidden");

			const success = await handleAdminLogin(password, remember);

			btn.disabled = false;
			btnText.textContent = "验证";
			btnLoading.classList.add("hidden");

			if (success) {
				document.getElementById("adminPassword").value = "";
			}
		});
	}
}

if (document.readyState === "loading") {
	document.addEventListener("DOMContentLoaded", bindAdminPasswordForm);
} else {
	bindAdminPasswordForm();
}

function getAuthHeaders() {
	const headers = {};
	if (adminPassword) {
		headers["X-Admin-Password"] = adminPassword;
	}
	return headers;
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
	if (isDark) {
		sun.classList.remove("hidden");
		moon.classList.add("hidden");
	} else {
		sun.classList.add("hidden");
		moon.classList.remove("hidden");
	}
}

document.getElementById("themeToggle").addEventListener("click", toggleTheme);

// --- Data Logic ---
function formatDate(dateStr) {
	if (!dateStr || dateStr.startsWith("0001")) return "-";
	const d = new Date(dateStr);
	return d.toLocaleDateString("zh-CN");
}

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

async function loadAccounts(isAutoRefresh = false) {
	try {
		const params = new URLSearchParams({
			page: currentState.page,
			size: currentState.size,
		});

		const resp = await fetch(`${API_BASE}/accounts?${params}`, {
			headers: getAuthHeaders(),
		});
		if (!resp.ok) throw new Error("Failed to fetch");

		const data = await resp.json();

		// Handle both old and new API response formats temporarily if needed, but we know it's new
		const items = data.items || [];
		const total = data.total || 0;

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
	} catch (e) {
		if (!isAutoRefresh) console.error("Failed to load accounts", e);
	}
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

	if (accounts.length === 0) {
		tbody.innerHTML = "";
		mobileList.innerHTML = "";
		tableContainer.classList.add("hidden");
		paginationContainer.classList.add("hidden");

		emptyState.classList.remove("hidden");
		emptyState.classList.add("flex");
		selectAll.checked = false;
		selectAll.disabled = true;
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
                <input type="checkbox" onchange="toggleSelect(${safeAccountId})" ${isSelected ? "checked" : ""} aria-label="选择账号 ${escapedAccountLabel}" class="rounded border-gray-300 dark:border-gray-600 text-primary focus:ring-primary h-4 w-4 mx-auto">
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center">
                <div class="text-sm font-medium text-gray-900 dark:text-white">${safeAccountId}</div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-left">
                <div class="flex items-center gap-2 min-w-[160px]">
                    <span class="inline-flex items-center rounded-md bg-blue-50 px-2 py-1 text-[10px] font-semibold text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-900/20 dark:text-blue-300 dark:ring-blue-400/30">
                        OAuth
                    </span>
                    <span class="max-w-[220px] truncate text-sm text-gray-700 dark:text-gray-200" title="${escapedAccountLabel}">${escapedAccountLabel}</span>
                </div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center">
                <div class="text-sm">
                    <span class="${lastUsedText === "从未" ? "text-gray-400" : "text-gray-600 dark:text-gray-300"}">${escapedLastUsedText}</span>
                    ${lastUsedTime ? `<div class="text-[10px] text-gray-400 mt-0.5">${escapeHtml(lastUsedTime)}</div>` : ""}
                </div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center text-sm">
                <div class="text-blue-600 dark:text-blue-400">今日 ${todayUsage}</div>
                <div class="text-xs text-gray-500 dark:text-gray-400 mt-1">累计 ${totalUsage}</div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-center text-sm font-medium">
                <div class="flex justify-center">
                    <button onclick="deleteAccount(${safeAccountId})" class="text-red-500 hover:text-red-700 focus:outline-none focus:ring-2 focus:ring-red-500 rounded-md transition-colors" title="删除" aria-label="删除账号 ${escapedAccountLabel}">
                        <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                        </svg>
                    </button>
                </div>
            </td>
        </tr>`;

		const mobile = `
        <article class="px-4 py-4 transition-colors ${isSelected ? "bg-blue-50 dark:bg-blue-900/10" : "bg-surface-light dark:bg-surface-dark"}">
            <div class="flex items-start gap-3">
                <input type="checkbox" onchange="toggleSelect(${safeAccountId})" ${isSelected ? "checked" : ""} aria-label="选择账号 ${escapedAccountLabel}" class="mt-1 rounded border-gray-300 dark:border-gray-600 text-primary focus:ring-primary h-4 w-4 shrink-0">
                <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-2">
                        <span class="inline-flex shrink-0 items-center rounded-md bg-blue-50 px-2 py-1 text-[10px] font-semibold text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-900/20 dark:text-blue-300 dark:ring-blue-400/30">OAuth</span>
                        <span class="truncate text-sm font-medium text-gray-900 dark:text-white" title="${escapedAccountLabel}">${escapedAccountLabel}</span>
                    </div>
                    <div class="mt-1 text-[11px] text-gray-400">账号 ID ${safeAccountId}</div>
                </div>
                <button onclick="deleteAccount(${safeAccountId})" class="-mr-1 rounded-lg p-2 text-red-500 hover:bg-red-50 hover:text-red-700 focus:outline-none focus:ring-2 focus:ring-red-500 dark:hover:bg-red-900/20 transition-colors" title="删除" aria-label="删除账号 ${escapedAccountLabel}">
                    <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                    </svg>
                </button>
            </div>
            <div class="mt-3 grid grid-cols-2 gap-3 border-t border-gray-100 pt-3 dark:border-gray-800">
                <div>
                    <div class="text-[10px] font-medium uppercase tracking-wider text-gray-400">最后使用</div>
                    <div class="mt-1 text-xs text-gray-600 dark:text-gray-300">${escapedLastUsedText}${lastUsedTime ? ` · ${escapeHtml(lastUsedTime)}` : ""}</div>
                </div>
                <div class="text-right">
                    <div class="text-[10px] font-medium uppercase tracking-wider text-gray-400">使用量</div>
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
	if (buttonsContainer) {
		buttonsContainer.innerHTML = buttonsHtml;
	}
}

function getBatchButtonsHtml(selectedCount) {
	const deleteHandler =
		selectedCount > 0 ? "batchDelete(false)" : "batchDelete(true)";
	return `
        <button onclick="${deleteHandler}" class="px-3 py-1.5 bg-red-700 text-white text-xs font-medium rounded-md hover:bg-red-800 transition-colors flex items-center gap-1">
            <svg xmlns="http://www.w3.org/2000/svg" class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
            </svg>
            ${selectedCount > 0 ? "删除选中" : "删除全部"}
        </button>
    `;
}

// 批量删除功能
async function batchDelete(deleteAll = false) {
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

	try {
		const resp = await fetch(`${API_BASE}/accounts/batch/delete`, {
			method: "POST",
			headers: {
				"Content-Type": "application/json",
				...getAuthHeaders(),
			},
			body: JSON.stringify(requestData),
		});

		if (!resp.ok) {
			const err = await resp.json();
			throw new Error(err.error || "Delete failed");
		}

		const result = await resp.json();
		alert(`成功删除 ${result.deleted_count || 0} 个账号`);

		// 清空选择并刷新页面
		currentState.selectedIds.clear();
		loadAccounts();
	} catch (e) {
		alert("批量删除失败: " + e.message);
	}
}

// --- Zencoder OAuth ---
function setOAuthButtonState(isWaiting) {
	const button = document.getElementById("oauthBtn");
	const text = document.getElementById("oauthBtnText");
	const icon = document.getElementById("oauthBtnIcon");
	const loading = document.getElementById("oauthBtnLoading");
	const status = document.getElementById("oauthStatus");

	if (!button || !text || !icon || !loading || !status) return;

	button.disabled = isWaiting;
	text.textContent = isWaiting ? "等待授权…" : "使用 Zencoder 登录";
	icon.classList.toggle("hidden", isWaiting);
	loading.classList.toggle("hidden", !isWaiting);
	status.classList.toggle("hidden", !isWaiting);
}

function resetOAuthFlow() {
	if (oauthFlow) {
		clearTimeout(oauthFlow.timeoutTimer);
		clearInterval(oauthFlow.closeTimer);
		if (oauthFlow.popup && !oauthFlow.popup.closed) {
			oauthFlow.popup.close();
		}
	}
	oauthFlow = null;
	setOAuthButtonState(false);
}

function failOAuthFlow(message) {
	resetOAuthFlow();
	showToast(message || "Zencoder 授权失败，请重试", "error");
}

function getPopupFeatures(width = 560, height = 720) {
	const left = Math.max(0, window.screenX + (window.outerWidth - width) / 2);
	const top = Math.max(0, window.screenY + (window.outerHeight - height) / 2);
	return `popup=yes,width=${width},height=${height},left=${Math.round(left)},top=${Math.round(top)},resizable=yes,scrollbars=yes`;
}

async function readResponseError(response, fallback) {
	try {
		const data = await response.json();
		if (typeof data.error === "string" && data.error.trim())
			return data.error.trim();
		if (typeof data.message === "string" && data.message.trim())
			return data.message.trim();
	} catch (_) {
		// The status code below is more useful than a non-JSON response body.
	}
	return `${fallback}（HTTP ${response.status}）`;
}

async function startZencoderOAuth() {
	if (oauthFlow) return;

	setOAuthButtonState(true);

	try {
		const response = await fetch(`${API_BASE}/oauth/zencoder/start`, {
			method: "POST",
			headers: getAuthHeaders(),
		});

		if (!response.ok) {
			throw new Error(
				await readResponseError(response, "无法启动 Zencoder 授权"),
			);
		}

		const data = await response.json();
		if (
			!data ||
			typeof data.authorization_url !== "string" ||
			!data.authorization_url.trim()
		) {
			throw new Error("服务器未返回有效的授权地址");
		}

		const authorizationURL = new URL(
			data.authorization_url,
			window.location.href,
		);
		if (!["http:", "https:"].includes(authorizationURL.protocol)) {
			throw new Error("服务器返回了不受支持的授权地址");
		}

		const popup = window.open(
			authorizationURL.href,
			"zencoder-oauth",
			getPopupFeatures(),
		);
		if (!popup) {
			window.location.assign(authorizationURL.href);
			return;
		}

		popup.focus();
		oauthFlow = {
			popup,
			state: typeof data.state === "string" ? data.state : null,
			timeoutTimer: null,
			closeTimer: null,
		};

		oauthFlow.timeoutTimer = setTimeout(() => {
			failOAuthFlow("Zencoder 授权已超时，请重新登录");
		}, OAUTH_TIMEOUT_MS);

		oauthFlow.closeTimer = setInterval(() => {
			if (oauthFlow && oauthFlow.popup.closed) {
				failOAuthFlow("授权窗口已关闭，账号尚未连接");
			}
		}, 600);
	} catch (error) {
		failOAuthFlow(`无法连接 Zencoder：${error.message || "请稍后重试"}`);
	}
}

async function handleOAuthMessage(event) {
	if (event.origin !== window.location.origin || !oauthFlow) return;
	if (oauthFlow.popup && event.source !== oauthFlow.popup) return;

	const data = event.data;
	if (
		!data ||
		data.type !== "zencoder-oauth" ||
		typeof data.success !== "boolean"
	)
		return;

	const message =
		typeof data.message === "string" && data.message.trim()
			? data.message.trim()
			: "";
	if (!data.success) {
		failOAuthFlow(message || "Zencoder 未完成授权，请重试");
		return;
	}

	resetOAuthFlow();
	currentState.page = 1;
	currentState.selectedIds.clear();
	await loadAccounts();
	showToast(message || "Zencoder 账号连接成功", "success");
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
window.addEventListener("message", handleOAuthMessage);

async function deleteAccount(id) {
	if (!confirm("确定要删除此账号吗？")) return;
	try {
		await fetch(`${API_BASE}/accounts/${id}`, {
			method: "DELETE",
			headers: getAuthHeaders(),
		});
		loadAccounts();
	} catch (e) {
		alert("删除失败");
	}
}

// Initialization function for after admin login
function initializeApp() {
	loadAccounts();

	// Auto Refresh
	if (autoRefreshTimer) {
		clearInterval(autoRefreshTimer);
	}
	autoRefreshTimer = setInterval(() => {
		loadAccounts(true);
	}, REFRESH_INTERVAL);
}

// Toast提示功能
function showToast(message, type = "info") {
	// 创建toast元素
	const toast = document.createElement("div");
	toast.className = `fixed top-4 right-4 px-6 py-3 rounded-lg shadow-lg transition-all duration-300 transform translate-x-96 z-50`;

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

	document.body.appendChild(toast);

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
	console.log("Page loaded, initializing...");
	initTheme();
	await initAdminAuth();
	consumeOAuthRedirectResult();
});
