// Copies the signed CSRF token cookie into every POST form and onto every htmx
// request. The token is a signed double-submit value: the server rejects any
// unsafe request whose submitted token does not match the cookie, so a
// cross-site page cannot forge one.
(function () {
	"use strict";

	function readCookie(name) {
		var parts = document.cookie ? document.cookie.split("; ") : [];
		for (var i = 0; i < parts.length; i++) {
			var part = parts[i];
			var eq = part.indexOf("=");
			if (eq > -1 && part.slice(0, eq) === name) {
				return decodeURIComponent(part.slice(eq + 1));
			}
		}
		return "";
	}

	function token() {
		return readCookie("csrf_token");
	}

	function inject(scope) {
		var value = token();
		if (!value) {
			return;
		}
		var forms = (scope || document).querySelectorAll("form");
		for (var i = 0; i < forms.length; i++) {
			var form = forms[i];
			var method = (form.getAttribute("method") || "get").toLowerCase();
			if (method !== "post") {
				continue;
			}
			if (form.querySelector('input[name="csrf_token"]')) {
				continue;
			}
			var input = document.createElement("input");
			input.type = "hidden";
			input.name = "csrf_token";
			input.value = value;
			form.appendChild(input);
		}
	}

	function init() {
		inject(document);
		// htmx (when used) must send the token as a header, since its requests
		// bypass normal form submission.
		document.body.addEventListener("htmx:configRequest", function (event) {
			var value = token();
			if (value && event.detail && event.detail.headers) {
				event.detail.headers["X-CSRF-Token"] = value;
			}
		});
		// fetch() calls to the JSON endpoints need the header too.
		if (window.fetch) {
			var originalFetch = window.fetch.bind(window);
			window.fetch = function (input, init) {
				init = init || {};
				var method = (init.method || (input && input.method) || "GET").toUpperCase();
				if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS") {
					var headers = new Headers(init.headers || (input && input.headers) || {});
					var value = token();
					if (value && !headers.has("X-CSRF-Token")) {
						headers.set("X-CSRF-Token", value);
					}
					init.headers = headers;
				}
				return originalFetch(input, init);
			};
		}
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", init);
	} else {
		init();
	}
})();
