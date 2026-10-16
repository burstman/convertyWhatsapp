// voiceComposer powers the web-inbox audio button: record from the microphone
// (OGG/Opus on Firefox, MP4/AAC on Chrome/Edge/Safari — the only formats
// WhatsApp accepts) or attach an audio file, preview, then POST multipart to
// /inbox/{conv}/reply-audio and swap in the re-rendered thread card.

// The text-send spinner is driven by a plain JS object (no Alpine store — the
// vendored Alpine build has no store/$store support), toggling the composer
// controls imperatively so the state survives the thread-card outerHTML swap.
// It stays armed until the sent bubble reports delivered/read/failed on a
// fragment refresh (90s safety timeout, and reset when switching conversations).
const composerSend = {
	pending: false,
	pendingMsgID: "",
	timeout: null,

	apply() {
		const card = document.getElementById("thread-card");
		if (!card) return;
		card.querySelectorAll(".composer-ctl").forEach((el) => {
			el.disabled = this.pending;
		});
		const arrow = card.querySelector("#composer-arrow");
		const spin = card.querySelector("#composer-spinner");
		const btn = card.querySelector("#composer-send-btn");
		if (arrow) arrow.style.display = this.pending ? "none" : "";
		if (spin) spin.style.display = this.pending ? "" : "none";
		if (btn) btn.title = this.pending ? "Delivering…" : "Send message";
	},

	markSending() {
		this.pending = true;
		this.pendingMsgID = "";
		clearTimeout(this.timeout);
		this.timeout = setTimeout(() => this.release(), 90000); // never stuck forever
		this.apply();
	},

	capture(msgID) {
		if (this.pending && !this.pendingMsgID && msgID) this.pendingMsgID = msgID;
	},

	seeStatus(status) {
		if (!this.pending || !status) return;
		if (status === "delivered" || status === "read" || status === "failed") this.release();
	},

	release() {
		this.pending = false;
		this.pendingMsgID = "";
		clearTimeout(this.timeout);
		this.timeout = null;
		this.apply();
	},
};
window.ComposerSend = composerSend;

// Scroll the bubble area to the bottom only when the reader was already near
// it, so the chat slides up for new messages without yanking a scrolled-up
// reader down.
function scrollIfNearBottom() {
	const el = document.getElementById("thread-bubbles");
	if (el && el.scrollTop + el.clientHeight >= el.scrollHeight - 260) {
		el.scrollTop = el.scrollHeight;
	}
}

// The reply POST, the 15s bubble poll and conversation nav all swap DOM under
// these targets; listen once on document, they survive every swap.
document.addEventListener("htmx:beforeRequest", (e) => {
	const form = e.target.closest && e.target.closest("form[hx-post]");
	if (form && form.getAttribute("hx-post").endsWith("/reply")) {
		composerSend.markSending();
	}
});

// A successful reply now returns an OOB bubble (hx-swap="none" keeps the card
// intact): clear the input, anchor the spinner on the appended bubble and let
// the chat slide up. Failed replies swap the whole card instead.
document.addEventListener("htmx:afterRequest", (e) => {
	if (!e.detail || !e.detail.successful || !e.detail.requestConfig) return;
	const el = e.detail.requestConfig.elt;
	if (!el || !el.matches || !el.matches("form[hx-post]")) return;
	const ta = document.getElementById("composer-input");
	if (ta) ta.value = "";
	if (ta) ta.dispatchEvent(new Event("input", { bubbles: true }));
	const bubbles = document.getElementById("thread-bubbles");
	const last = bubbles && bubbles.querySelector(".flex[data-direction='outbound']:last-of-type");
	if (last) {
		composerSend.capture(last.getAttribute("data-msg-id"));
		composerSend.seeStatus(last.getAttribute("data-status"));
	}
	scrollIfNearBottom();
});

document.addEventListener("htmx:afterSwap", (e) => {
	const t = e.detail && e.detail.target;
	if (!t || !t.id) return;
	if (t.id === "thread-card") {
		// Full-card re-render (failed reply): the newest outbound bubble is ours.
		const last = t.querySelector("#thread-bubbles .flex[data-direction='outbound']:last-of-type");
		if (last) {
			composerSend.capture(last.getAttribute("data-msg-id"));
			composerSend.seeStatus(last.getAttribute("data-status"));
		}
		composerSend.apply(); // the fresh card's controls inherit the pending state
	} else if (t.id === "thread-bubbles" && composerSend.pending && composerSend.pendingMsgID) {
		const want = composerSend.pendingMsgID;
		const node = t.querySelector('[data-msg-id="' + want + '"]');
		if (node) composerSend.seeStatus(node.getAttribute("data-status"));
		composerSend.apply();
	} else if (t.id === "inbox-thread-pane") {
		// A different conversation was opened; drop any pending send state.
		composerSend.release();
	}
});

// Avoid a double submit while a send is pending (textarea sits outside the
// form's htmx disabled logic, which lives on the DOM-adjacent controls).
document.addEventListener("keydown", (e) => {
	if (e.key !== "Enter" || e.shiftKey || !e.target || e.target.id !== "composer-input") return;
	e.preventDefault();
	if (!composerSend.pending && e.target.form) e.target.form.requestSubmit();
});
window.voiceComposer = (convID) => ({
	state: "idle", // idle | recording | ready
	mime: "",
	isImage: false,
	canRecord: false,
	recorder: null,
	stream: null,
	chunks: [],
	timer: null,
	timerStart: 0,
	durationMs: 0,
	blob: null,
	url: null,
	errors: "",
	sending: false,

	init() {
		const wanted = ["audio/ogg;codecs=opus", "audio/ogg", "audio/mp4", "audio/mpeg"];
		this.mime =
			(window.MediaRecorder &&
				wanted.find((m) => {
					try {
						return MediaRecorder.isTypeSupported(m);
					} catch {
						return false;
					}
				})) ||
			"";
		this.canRecord = this.mime !== "";
	},

	start() {
		if (!this.canRecord) {
			this.errors = "Recording is not supported in this browser — attach an audio file (MP3, M4A, OGG, AAC) instead.";
			return;
		}
		if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
			this.errors = "Microphone access is not available in this browser.";
			return;
		}
		clearInterval(this.timer);
		this.chunks = [];
		navigator.mediaDevices
			.getUserMedia({ audio: true })
			.then((stream) => {
				this.stream = stream;
				this.recorder = new MediaRecorder(stream, { mimeType: this.mime });
				this.recorder.ondataavailable = (e) => {
					if (e.data && e.data.size) this.chunks.push(e.data);
				};
				this.recorder.onstop = () => {
					this.stream.getTracks().forEach((t) => t.stop());
					this.blob = new Blob(this.chunks, { type: this.recorder.mimeType || this.mime });
					if (this.url) URL.revokeObjectURL(this.url);
					this.url = URL.createObjectURL(this.blob);
					this.state = "ready";
					clearInterval(this.timer);
				};
				this.recorder.start(250);
				this.timerStart = Date.now();
				this.timer = setInterval(() => {
					this.durationMs = Date.now() - this.timerStart;
				}, 333);
				this.state = "recording";
				this.errors = "";
			})
			.catch(() => {
				this.errors = "Microphone access was denied — allow it, or attach an audio file instead.";
			});
	},

	stop() {
		if (this.recorder && this.recorder.state === "recording") this.recorder.stop();
	},

	cancel() {
		if (this.recorder && this.recorder.state === "recording") {
			this.recorder.onstop = null;
			this.recorder.stop();
		}
		if (this.stream) this.stream.getTracks().forEach((t) => t.stop());
		this.reset();
	},

	reset() {
		clearInterval(this.timer);
		clearTimeout(this.durLoad);
		if (this.url) URL.revokeObjectURL(this.url);
		this.state = "idle";
		this.recorder = null;
		this.stream = null;
		this.chunks = [];
		this.blob = null;
		this.url = null;
		this.durationMs = 0;
		this.isImage = false;
	},

	async loadFile(input) {
		const f = input && input.files && input.files[0];
		if (!f) return;
		const audioMime = this.canonical(f.type);
		const imageMime = this.canonicalImage(f.type);
		const mime = audioMime || imageMime;
		if (!mime) {
			this.errors = "WhatsApp does not accept this format (WebM is not supported). Use a JPG, PNG or WEBP image, or an MP3, M4A, OGG, AAC, AMR audio file.";
			return;
		}
		if (audioMime && f.size > 16 * 1024 * 1024) {
			this.errors = "This audio file is larger than the 16 MB WhatsApp limit.";
			return;
		}
		let blob = f;
		let outMime = mime;
		if (imageMime && f.size > 5 * 1024 * 1024) {
			this.errors = "Compressing photo…";
			const compressed = await this.compressImage(f);
			if (!compressed) {
				this.errors = "Could not compress this photo below 5 MB. Pick a smaller JPG, PNG or WEBP.";
				return;
			}
			blob = compressed;
			outMime = "image/jpeg";
		}
		this.mime = outMime;
		this.isImage = outMime.indexOf("image/") === 0;
		this.blob = blob;
		if (this.url) URL.revokeObjectURL(this.url);
		this.url = URL.createObjectURL(blob);
		this.state = "ready";
		this.errors = "";
		this.durationMs = 0;
		clearTimeout(this.durLoad);
		if (this.isImage) return;
		const probe = new Audio();
		this.durLoad = setTimeout(() => {
			probe.src = this.url;
			probe.onloadedmetadata = () => {
				if (Number.isFinite(probe.duration) && probe.duration > 0) {
					this.durationMs = Math.round(probe.duration * 1000);
				}
			};
		}, 30);
	},

	// compressImage downsizes an oversized photo onto a canvas (max 2048px,
	// WhatsApp-friendly) and re-encodes it as JPEG, stepping quality down until
	// it fits under the 5 MB Cloud API limit.
	compressImage(file) {
		return new Promise((resolve) => {
			const img = new Image();
			const url = URL.createObjectURL(file);
			img.onload = () => {
				const maxSide = 2048;
				let w = img.naturalWidth;
				let h = img.naturalHeight;
				const scale = Math.min(1, maxSide / Math.max(w, h));
				w = Math.max(1, Math.round(w * scale));
				h = Math.max(1, Math.round(h * scale));
				const canvas = document.createElement("canvas");
				canvas.width = w;
				canvas.height = h;
				const ctx = canvas.getContext("2d");
				ctx.fillStyle = "#ffffff";
				ctx.fillRect(0, 0, w, h);
				ctx.drawImage(img, 0, 0, w, h);
				URL.revokeObjectURL(url);
				const qualities = [0.9, 0.82, 0.72, 0.62, 0.5, 0.38];
				const step = () => {
					if (qualities.length === 0) {
						resolve(null);
						return;
					}
					const q = qualities.shift();
					canvas.toBlob((out) => {
						if (!out) {
							resolve(null);
							return;
						}
						if (out.size <= 5 * 1024 * 1024) {
							resolve(out);
						} else {
							step();
						}
					}, "image/jpeg", q);
				};
				step();
			};
			img.onerror = () => {
				URL.revokeObjectURL(url);
				resolve(null);
			};
			img.src = url;
		});
	},

	canonical(mime) {
		const base = String(mime || "").split(";")[0].trim().toLowerCase();
		if (["audio/ogg", "audio/opus"].includes(base)) return "audio/ogg";
		if (["audio/mp4", "audio/m4a", "audio/x-m4a", "audio/m4b"].includes(base)) return "audio/mp4";
		if (["audio/mpeg", "audio/mp3", "audio/mpga"].includes(base)) return "audio/mpeg";
		if (base === "audio/aac") return "audio/aac";
		if (["audio/amr", "audio/amr-nb", "audio/x-amr"].includes(base)) return "audio/amr";
		return "";
	},

	canonicalImage(mime) {
		const base = String(mime || "").split(";")[0].trim().toLowerCase();
		if (["image/jpg", "image/jpeg", "image/pjpeg"].includes(base)) return "image/jpeg";
		if (base === "image/png") return "image/png";
		if (base === "image/webp") return "image/webp";
		return "";
	},

	ext() {
		if (this.isImage) {
			const imgs = { "image/jpeg": "jpg", "image/png": "png", "image/webp": "webp" };
			return imgs[this.mime] || "jpg";
		}
		const exts = { "audio/ogg": "ogg", "audio/mp4": "m4a", "audio/mpeg": "mp3", "audio/aac": "aac", "audio/amr": "amr" };
		return exts[this.mime] || "mp3";
	},

	fmt() {
		const s = Math.floor(this.durationMs / 1000);
		const m = Math.floor(s / 60);
		return String(m).padStart(2, "0") + ":" + String(s % 60).padStart(2, "0");
	},

	send() {
		if (!this.blob || this.sending) return;
		this.sending = true;
		this.errors = "";
		const fd = new FormData();
		const endpoint = this.isImage ? "/inbox/" + convID + "/reply-image" : "/inbox/" + convID + "/reply-audio";
		fd.append(this.isImage ? "image" : "audio", this.blob, (this.isImage ? "photo." : "voice.") + this.ext());
		if (!this.isImage) fd.append("duration_ms", String(this.durationMs));
		fetch(endpoint, { method: "POST", body: fd })
			.then((res) => res.text())
			.then((html) => {
				if (html.indexOf("<") !== 0 && html.trim() !== "") {
					this.errors = this.friendlyError(html);
					return;
				}
				const frag = document.createElement("template");
				frag.innerHTML = html;
				const oob = frag.content.querySelector("[hx-swap-oob]");
				const bubbles = document.getElementById("thread-bubbles");
				if (oob && bubbles) {
					oob.removeAttribute("hx-swap-oob");
					bubbles.appendChild(oob);
					scrollIfNearBottom();
				}
				this.reset();
			})
			.catch(() => {
				this.errors = "Could not reach the server. Try again.";
			})
			.finally(() => {
				this.sending = false;
			});
	},

	friendlyError(text) {
		const t = String(text || "").trim();
		if (!t) return "Could not send. Try again.";
		if (t.indexOf("<!DOCTYPE") === 0 || t.indexOf("<html") === 0) return "Could not send. Try again.";
		if (t.length > 200) return t.slice(0, 200) + "…";
		return t;
	},
});