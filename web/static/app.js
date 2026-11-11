"use strict";

(function () {
  const csrf = document.querySelector('meta[name="csrf-token"]')?.content || "";
  const form = document.getElementById("upload-form");
  const input = document.getElementById("file-input");
  const dropzone = document.getElementById("dropzone");
  const statusList = document.getElementById("upload-status");
  const gallery = document.getElementById("gallery");

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function status(name) {
    if (!statusList) return null;
    const li = el("li");
    const label = el("span", "", name);
    const state = el("span", "muted", "等待中");
    li.append(label, state);
    statusList.append(li);
    return { li, state };
  }

  function setStatus(row, text, cls) {
    if (!row) return;
    row.state.textContent = text;
    row.state.className = cls || "muted";
  }

  function prependCard(img) {
    if (!gallery) return false;
    if (img.id && gallery.querySelector(`figure[data-id="${CSS.escape(img.id)}"]`)) {
      return false;
    }
    document.getElementById("gallery-empty")?.remove();
    const figure = el("figure", "thumb");
    figure.dataset.id = img.id;
    const link = el("a");
    link.href = img.url;
    link.target = "_blank";
    link.rel = "noopener";
    const image = el("img");
    image.src = img.url;
    image.alt = img.name || "";
    image.loading = "lazy";
    link.append(image);
    const caption = el("figcaption");
    const name = el("span", "name", img.name || "image");
    name.title = img.name || "";
    const meta = el("span", "meta", `${img.width}×${img.height} · ${formatBytes(img.size)} · 刚刚`);
    const actions = el("span", "actions");
    actions.append(
      copyButton("复制链接", img.url),
      copyButton("Markdown", img.markdown || `![${img.name || "image"}](${img.url})`),
      deleteButton(img.id)
    );
    caption.append(name, meta, actions);
    figure.append(link, caption);
    gallery.prepend(figure);
    return true;
  }

  function adjustImageCount(delta) {
    const node = document.getElementById("image-count");
    if (!node) return;
    const current = Number.parseInt(node.textContent, 10) || 0;
    node.textContent = String(Math.max(0, current + delta));
  }

  function copyButton(label, text) {
    const b = el("button", "", label);
    b.type = "button";
    b.dataset.copy = text;
    return b;
  }

  function deleteButton(id) {
    const b = el("button", "danger", "删除");
    b.type = "button";
    b.dataset.delete = id;
    return b;
  }

  function formatBytes(n) {
    if (!Number.isFinite(n)) return "";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
  }

  function uploadFile(file) {
    const row = status(`${file.name} · ${formatBytes(file.size)}`);
    if (!row) return Promise.resolve();
    const progress = el("progress", "progress");
    progress.max = 100;
    progress.value = 0;
    row.li.append(progress);

    return new Promise((resolve) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", "/api/v1/upload");
      xhr.setRequestHeader("X-CSRF-Token", csrf);
      // FormData carries the Unicode filename. HTTP header values cannot
      // represent arbitrary Unicode (setRequestHeader throws for Chinese or
      // emoji names), so do not duplicate the filename in X-Filename.
      xhr.responseType = "json";
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) {
          const pct = Math.round((e.loaded / e.total) * 100);
          progress.value = pct;
          setStatus(row, pct + "%");
        }
      };
      xhr.onload = () => {
        const body = xhr.response;
        if (xhr.status === 200 || xhr.status === 201) {
          setStatus(row, body.deduplicated ? "已存在，返回原链接" : "上传成功", "ok");
          progress.remove();
          const added = prependCard(body);
          if (!body.deduplicated && added) adjustImageCount(1);
          refreshMe();
        } else {
          const msg = body?.error?.message || `失败（HTTP ${xhr.status}）`;
          setStatus(row, msg, "err");
          progress.remove();
        }
        resolve();
      };
      xhr.onerror = () => {
        setStatus(row, "网络错误", "err");
        progress.remove();
        resolve();
      };
      const data = new FormData();
      data.append("file", file, file.name);
      xhr.send(data);
    });
  }

  async function uploadFiles(files) {
    // Some browsers/OS combinations leave File.type empty even for real
    // images. The server sniffs the bytes, so only reject a non-empty,
    // explicitly non-image type here.
    const list = Array.from(files || []).filter((f) => !f.type || f.type.startsWith("image/"));
    if (list.length === 0) {
      if (files && files.length) alert("只支持图片文件（JPEG / PNG / GIF / WebP）。");
      return;
    }
    for (const file of list) {
      await uploadFile(file);
    }
  }

  async function refreshMe() {
    try {
      const resp = await fetch("/api/v1/me", { headers: { "Accept": "application/json" } });
      if (!resp.ok) return;
      const me = await resp.json();
      const quota = document.querySelector(".quota");
      if (quota) {
        quota.querySelector("span").textContent = `${formatBytes(me.used_bytes)} / ${formatBytes(me.quota_bytes)}`;
        const pct = me.quota_bytes ? Math.min(100, (me.used_bytes / me.quota_bytes) * 100) : 0;
        quota.querySelector(".bar").value = pct;
      }
    } catch (_) { /* ignore */ }
  }

  if (dropzone && input) {
    // Keep the native picker visible when JS is disabled or fails to load.
    input.hidden = true;
    dropzone.addEventListener("click", (e) => {
      if (e.target !== input) input.click();
    });
    dropzone.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); input.click(); }
    });
    input.addEventListener("change", () => {
      if (form && form.dataset.js === "1") {
        uploadFiles(input.files);
        input.value = "";
      }
    });
    ["dragenter", "dragover"].forEach((ev) =>
      dropzone.addEventListener(ev, (e) => { e.preventDefault(); dropzone.classList.add("dragover"); })
    );
    ["dragleave", "drop"].forEach((ev) =>
      dropzone.addEventListener(ev, (e) => { e.preventDefault(); dropzone.classList.remove("dragover"); })
    );
    dropzone.addEventListener("drop", (e) => uploadFiles(e.dataTransfer.files));
  }

  if (form) {
    form.dataset.js = "1";
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (input && input.files && input.files.length) {
        uploadFiles(input.files);
        input.value = "";
      } else {
        alert("请先选择图片。");
      }
    });
  }

  document.addEventListener("paste", (e) => {
    if (!form) return;
    const files = Array.from(e.clipboardData?.files || []);
    if (files.length) {
      e.preventDefault();
      uploadFiles(files);
    }
  });

  document.addEventListener("click", async (e) => {
    const copy = e.target.closest("[data-copy]");
    if (copy) {
      const text = copy.dataset.copy;
      try {
        await navigator.clipboard.writeText(text);
      } catch (_) {
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.append(ta);
        ta.select();
        document.execCommand("copy");
        ta.remove();
      }
      const old = copy.textContent;
      copy.textContent = "已复制";
      setTimeout(() => { copy.textContent = old; }, 1200);
      return;
    }

    const del = e.target.closest("[data-delete]");
    if (del) {
      const id = del.dataset.delete;
      if (!confirm("删除这张图片？CDN 缓存刷新可能需要一点时间。")) return;
      del.disabled = true;
      try {
        const resp = await fetch(`/api/v1/images/${encodeURIComponent(id)}`, {
          method: "DELETE",
          headers: { "X-CSRF-Token": csrf },
        });
        if (resp.ok || resp.status === 204) {
          const card = document.querySelector(`figure[data-id="${CSS.escape(id)}"]`);
          if (card) {
            card.remove();
            adjustImageCount(-1);
          }
          refreshMe();
          if (gallery && !gallery.querySelector("figure")) {
            const empty = el("p", "empty", "还没有图片，上传第一张吧。");
            empty.id = "gallery-empty";
            gallery.append(empty);
          }
        } else {
          const body = await resp.json().catch(() => null);
          alert(body?.error?.message || `删除失败（HTTP ${resp.status}）`);
          del.disabled = false;
        }
      } catch (_) {
        alert("网络错误，删除失败。");
        del.disabled = false;
      }
    }
  });

  document.getElementById("refresh-gallery")?.addEventListener("click", () => location.reload());
})();
