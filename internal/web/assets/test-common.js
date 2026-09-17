"use strict";
/* Shared runtime for the ASS per-service test pages.
 *
 * A page supplies the form markup (in #form) and a builder:
 *
 *     window.ASS_TEST = {
 *       build(root) -> { params, files:[{field,file,filename}], encoding }
 *     };
 *
 * `encoding` picks how the body is sent to ASS (which forwards it verbatim to
 * the backend, so it must be the shape THAT backend's parser wants):
 *   "json"     - application/json, body = JSON(params). No files.
 *   "blob"     - multipart; params as one JSON field "params" + file fields.
 *                (demucs / SA3 forks read this.)
 *   "paramobj" - multipart; params as one JSON field "param_obj" + file fields.
 *                (ACE-Step's RequestParser unpacks param_obj; keeps types.)
 *
 * build() throws on bad/missing input; the message gets toasted. Everything
 * below — the submit dance, polling, artifact rendering, the drop zones and
 * segmented toggles — is service-agnostic and lives here once. */

const $ = s => document.querySelector(s);
const SERVICE = decodeURIComponent(location.pathname.split("/").pop());
const files = {}; // multipart field name -> selected File
let polling = false;

// --- little utilities ------------------------------------------------------
function toast(msg, isErr) {
  const el = $("#toast"); el.textContent = msg; el.classList.toggle("err", !!isErr);
  el.classList.add("show"); clearTimeout(el._t); el._t = setTimeout(() => el.classList.remove("show"), 3600);
}
function fmtBytes(n){ if(n==null) return "—"; const u=["B","KB","MB","GB"]; let i=0,v=n; while(v>=1024&&i<u.length-1){v/=1024;i++;} return v.toFixed(v<10&&i>0?1:0)+u[i]; }
function esc(s){ return String(s).replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c])); }

// --- tiny form helpers (used by page build() fns) --------------------------
const val = (r,p) => { const el=r.querySelector(`[data-p="${p}"]`); return el?el.value.trim():""; };
const numOpt = (r,p) => { const v=val(r,p); return v===""?undefined:Number(v); };
const checked = (r,p) => { const el=r.querySelector(`[data-p="${p}"]`); return el?el.checked:false; };
const seg = (r,name) => { const on=r.querySelector(`[data-seg="${name}"] .on`); return on?on.dataset.v:""; };
const getFile = f => files[f] || null;
// Parse a comma/space list of numbers ("8, 16" -> [8,16]); "" -> null.
function numList(s){ const t=(s||"").trim(); if(!t) return null; return t.split(/[,\s]+/).filter(Boolean).map(x=>{ const n=Number(x); if(isNaN(n)) throw new Error(`"${x}" is not a number`); return n; }); }
// expose to page scripts
Object.assign(window, { $, esc, val, numOpt, checked, seg, getFile, numList, toast, fmtBytes });

// A file drop zone, addressed by its multipart field name. Pages call this in
// their form HTML string.
function dropHTML(label, field, optional){
  return `
    <div class="field">
      <label>${esc(label)}${optional?" (optional)":""}</label>
      <div class="drop" data-field="${field}">
        <div>Drop a file here or <b>browse</b></div>
        <div class="chosen" hidden></div>
        <input type="file" accept="audio/*,.wav,.flac,.mp3,.m4a,.ogg" hidden>
      </div>
    </div>`;
}
window.dropHTML = dropHTML;

// --- wiring ----------------------------------------------------------------
function wireSegments(root){
  root.querySelectorAll("[data-seg]").forEach(sg => {
    sg.querySelectorAll("button").forEach(b => b.onclick = () => {
      sg.querySelectorAll("button").forEach(x => x.classList.remove("on"));
      b.classList.add("on");
      // A segmented toggle named "workflow" (or "task") reveals matching [data-wf] sections.
      if (sg.dataset.seg === "workflow" || sg.dataset.seg === "task") applyWorkflow(root, b.dataset.v);
    });
  });
}
function wireDrops(root){
  root.querySelectorAll(".drop").forEach(drop => {
    const field = drop.dataset.field;
    const input = drop.querySelector("input[type=file]");
    const chosen = drop.querySelector(".chosen");
    const set = f => { files[field]=f||null; if(f){ chosen.hidden=false; chosen.textContent=`${f.name} · ${fmtBytes(f.size)}`; } else { chosen.hidden=true; } };
    drop.onclick = () => input.click();
    input.onchange = () => set(input.files[0]);
    ["dragover","dragenter"].forEach(e => drop.addEventListener(e, ev => { ev.preventDefault(); drop.classList.add("over"); }));
    ["dragleave","drop"].forEach(e => drop.addEventListener(e, ev => { ev.preventDefault(); drop.classList.remove("over"); }));
    drop.addEventListener("drop", ev => { if(ev.dataTransfer.files[0]) set(ev.dataTransfer.files[0]); });
  });
}
// Show only the active workflow/task section.
function applyWorkflow(root, wf){
  root.querySelectorAll("[data-wf]").forEach(sec => sec.classList.toggle("show", (sec.dataset.wf||"").split(/\s+/).includes(wf)));
}
window.applyWorkflow = applyWorkflow;

// --- lifecycle -------------------------------------------------------------
async function init(){
  $("#svcname").textContent = SERVICE;
  let verb = null;
  try {
    const r = await fetch("/v1/backends");
    const data = await r.json();
    const b = (data.backends||[]).find(x => x.name === SERVICE);
    verb = b ? b.verb : null;
  } catch(e) { /* offline is fine; the page still submits */ }
  if ($("#svcverb")) $("#svcverb").textContent = verb ? (verb + " · test") : "test";

  wireSegments($("#form"));
  wireDrops($("#form"));
  // Let the page do any first-render toggling once its DOM is wired.
  if (window.ASS_TEST && typeof window.ASS_TEST.ready === "function") window.ASS_TEST.ready($("#form"));
  $("#submit").disabled = false;
}

async function submit(){
  if (polling) return;
  if (!window.ASS_TEST || typeof window.ASS_TEST.build !== "function") { toast("page has no builder", true); return; }
  let req;
  try { req = window.ASS_TEST.build($("#form")); }
  catch(e){ toast(e.message, true); return; }

  let body, headers;
  const enc = req.encoding || "blob";
  if (enc === "json") {
    body = JSON.stringify(req.params);
    headers = { "Content-Type": "application/json" };
  } else {
    const fd = new FormData();
    fd.append(enc === "paramobj" ? "param_obj" : "params", JSON.stringify(req.params));
    (req.files||[]).forEach(f => fd.append(f.field, f.file, f.filename || f.file.name));
    body = fd; headers = undefined; // let the browser set the multipart boundary
  }

  $("#resultpanel").hidden = false;
  $("#art").innerHTML = "";
  setStatus("submitting…", "spin");
  $("#submit").disabled = true;
  try {
    const r = await fetch(`/v1/${encodeURIComponent(SERVICE)}/jobs`, { method:"POST", body, headers });
    const jb = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(jb.error || ("HTTP " + r.status));
    await poll(jb.job_id);
  } catch(e) {
    setStatus("submit failed: " + e.message, "err");
    toast(e.message, true);
    $("#submit").disabled = false;
  }
}

function setStatus(text, kind){
  const el = $("#status");
  const spin = kind === "spin" ? '<span class="spin"></span>' : "";
  el.className = "status" + (kind==="ok"?" ok":kind==="err"?" err":"");
  el.innerHTML = spin + `<span>${esc(text)}</span>`;
}

async function poll(jobID){
  polling = true;
  const started = Date.now();
  while (true) {
    let job;
    try {
      const r = await fetch(`/v1/jobs/${jobID}`);
      job = await r.json();
      if (!r.ok) throw new Error(job.error || ("HTTP " + r.status));
    } catch(e){ setStatus("poll failed: " + e.message, "err"); break; }
    const secs = Math.round((Date.now()-started)/1000);
    if (job.state === "succeeded") { setStatus(`succeeded in ${secs}s`, "ok"); renderArtifacts(jobID, job.artifacts||[]); break; }
    if (job.state === "failed")    { setStatus("failed: " + (job.error||"unknown"), "err"); break; }
    setStatus(`${job.state}…  ${secs}s`, "spin");
    await new Promise(r => setTimeout(r, 1500));
  }
  polling = false;
  $("#submit").disabled = false;
}

function renderArtifacts(jobID, arts){
  const box = $("#art");
  if (!arts.length) { box.innerHTML = '<div class="muted">Job produced no artifacts.</div>'; return; }
  box.innerHTML = "";
  arts.forEach(a => {
    const url = `/v1/jobs/${jobID}/result/${encodeURIComponent(a.name)}`;
    const ct = a.content_type || "";
    const isAudio = ct.startsWith("audio/");
    const isImage = ct.startsWith("image/");
    const isText  = ct.startsWith("text/") || ct === "application/json";
    const el = document.createElement("div");
    el.className = "artcard";
    el.innerHTML = `
      <div class="arthead">
        <span class="aname">${esc(a.name)}</span>
        <span class="akind">${esc(a.kind || "file")}</span>
        <span class="asize">${fmtBytes(a.bytes)}</span>
      </div>
      ${isAudio ? `<audio controls preload="none" src="${url}"></audio>` : ""}
      ${isImage ? `<img src="${url}" alt="${esc(a.name)}" style="max-width:100%;border-radius:8px;display:block;margin-bottom:8px;">` : ""}
      ${isText ? `<pre data-textsrc="${url}">loading…</pre>` : ""}
      <a class="dl" href="${url}" download="${esc(a.name)}">↓ download ${esc(ct)}</a>`;
    box.appendChild(el);
  });
  // Fill text previews (lyrics, audio_codes, metadata) inline — small, handy.
  box.querySelectorAll("pre[data-textsrc]").forEach(async pre => {
    try {
      const r = await fetch(pre.dataset.textsrc);
      let t = await r.text();
      if ((pre.dataset.textsrc||"").endsWith(".json")) { try { t = JSON.stringify(JSON.parse(t), null, 2); } catch(_){} }
      pre.textContent = t.length > 20000 ? t.slice(0,20000) + "\n… (truncated)" : t;
    } catch(e){ pre.textContent = "(failed to load: " + e.message + ")"; }
  });
}

window.addEventListener("DOMContentLoaded", () => {
  $("#submit").onclick = submit;
  init();
});
