const $ = (s, root = document) => root.querySelector(s);
const form = $('#draft'), id = form.dataset.id, api = `/api/albums/${id}`;
const fields = $('#draft-fields'), submit = $('#create-album'), stop = $('#stop-create');
const main = $('#main'), picker = $('.draft-picker'), pick = $('#draft-files'), add = $('#pick-tracks');
const cover = $('#draft-cover'), chooseCover = $('#pick-cover'), clear = $('#clear-cover');
const art = $('#draft-art'), glow = $('#draft-glow');
const artNote = $('#draft-art-note'), coverName = $('#draft-cover-name'), coverError = $('#draft-cover-error');
const status = $('#draft-status'), error = $('#draft-error'), trackError = $('#draft-track-error');
const list = $('#draft-tracks'), tracks = new Map();
let request, pending, done, coverFile;

function unload(e) { e.preventDefault(); e.returnValue = ''; }
const dirty = () => addEventListener('beforeunload', unload);
form.oninput = e => { if (e.target.type !== 'file') dirty(); };
$('#cancel-draft').onclick = e => { e.preventDefault(); location.replace('/'); };
$('#draft-existing').onclick = e => {
  e.preventDefault();
  removeEventListener('beforeunload', unload);
  location.replace(e.currentTarget.href);
};
const { artist_choice: choice, artist_name: name } = form.elements;
choice.onchange = () => { name.disabled = $('#draft-artist').hidden = choice.value !== 'name'; };
choice.onchange();

function notice(message, details) {
  $('p', error).textContent = message;
  const disclosure = $('details', error);
  disclosure.open = false;
  disclosure.hidden = !details || !Object.keys(details).length;
  $('pre', disclosure).textContent = disclosure.hidden ? '' : JSON.stringify(details, null, 2);
  error.hidden = false;
  error.focus();
}
function trackNotice(message) {
  trackError.textContent = message;
  trackError.hidden = false;
  trackError.focus();
}
function showTracks() {
  const count = tracks.size;
  $('#draft-count').textContent = `${count} ${count === 1 ? 'track' : 'tracks'}`;
  add.textContent = count ? 'Add more audio files' : 'Add audio files';
  list.hidden = $('#draft-inference').hidden = !count;
  trackError.hidden = true;
}
function addTracks(files) {
  if (fields.disabled) return;
  const names = new Set(tracks.keys());
  for (const { name, size } of files) {
    if (!size || !/\.(flac|mp3|m4a)$/i.test(name)) {
      return trackNotice(`${name}: Choose non-empty FLAC, MP3 or M4A files. No tracks were added from this selection.`);
    }
    if (names.has(name)) return trackNotice(`Duplicate filename: ${name}. No tracks were added from this selection.`);
    names.add(name);
  }
  if (!files.length) return;
  dirty();
  for (const file of files) {
    const row = $('#draft-track').content.firstElementChild.cloneNode(true);
    $('legend', row).textContent = $('.file-name', row).textContent = file.name;
    $('summary', row).setAttribute('aria-label', `Track details for ${file.name}`);
    const genre = $('[name=genre]', row), noGenre = $('[name=nogenre]', row);
    noGenre.onchange = () => { genre.disabled = noGenre.checked; };
    const remove = $('button', row);
    $('.sr-only', remove).textContent = ` ${file.name}`;
    remove.onclick = () => {
      dirty();
      const next = row.nextElementSibling || row.previousElementSibling;
      tracks.delete(file.name);
      row.remove();
      showTracks();
      (next ? $('button', next) : add).focus();
    };
    tracks.set(file.name, { file, row });
    list.append(row);
  }
  showTracks();
}
add.onclick = () => pick.click();
pick.onchange = () => {
  const files = [...pick.files];
  pick.value = '';
  addTracks(files);
};

// Cancel file and link navigation even outside the draft or while locked.
const urlDragTypes = new Set(['text/uri-list', 'url', 'text/x-moz-url', 'text/x-moz-url-data',
  'text/url', 'public.url', 'public.file-url']);
let dragTarget, dropZone;
function showDrop(zone = null, target = null) {
  dragTarget = target;
  if (zone === dropZone) return;
  dropZone?.classList.remove('is-drop');
  dropZone = zone;
  dropZone?.classList.add('is-drop');
}
function coverNotice(message) {
  coverError.textContent = `${message} The cover selection hasn’t changed.`;
  coverError.hidden = false;
  coverError.focus();
}
for (const type of ['dragenter', 'dragover', 'dragleave', 'drop']) {
  document.addEventListener(type, e => {
    const data = e.dataTransfer;
    if (!data) return;
    if (!data.types.includes('Files')) {
      if (type !== 'dragleave' && data.types.some(t => urlDragTypes.has(t.toLowerCase()))) {
        e.preventDefault();
        data.dropEffect = 'none';
        showDrop();
      }
      return;
    }
    if (type === 'dragleave') {
      // A child may be entered before the previous target receives its leave.
      if (e.target === dragTarget && !dropZone?.contains(e.relatedTarget)) showDrop();
      return;
    }
    e.preventDefault();
    const zone = fields.disabled ? null : e.target.closest?.('.draft-picker, #draft-cover-drop');
    if (type !== 'drop') {
      data.dropEffect = zone ? 'copy' : 'none';
      showDrop(zone || null, zone ? e.target : null);
      return;
    }
    showDrop();
    if (!zone) return;
    const folder = [...data.items].some(item => item.webkitGetAsEntry?.()?.isDirectory);
    if (zone === picker) {
      if (folder) return trackNotice('Drop individual audio files, not folders. No tracks were added.');
      addTracks(data.files);
    } else if (folder || data.files.length !== 1) {
      coverNotice('Drop one JPEG or PNG file at a time.');
    } else {
      selectCover(data.files[0]);
    }
  }, true);
}

function paint(canvas, bitmap, max) {
  const scale = Math.min(1, max / Math.max(bitmap.width, bitmap.height));
  canvas.width = Math.max(1, Math.round(bitmap.width * scale));
  canvas.height = Math.max(1, Math.round(bitmap.height * scale));
  canvas.getContext('2d').drawImage(bitmap, 0, 0, canvas.width, canvas.height);
}
let coverVersion = 0, glowVersion = 0;
function fadeGlow() {
  if (main.getAttribute('data-glow') !== '') return;
  const version = glowVersion;
  main.setAttribute('data-glow', 'fading');
  const release = () => {
    if (version !== glowVersion) return;
    glow.width = glow.height = 1;
    main.removeAttribute('data-glow');
  };
  // Both layers have the same opacity timing. CSS exposes no transition
  // here under reduced motion, or if the glow was not yet visible.
  const [fade] = glow.getAnimations();
  if (fade) fade.finished.then(release, release);
  else release();
}
async function previewCover(file, version) {
  let bitmap;
  try {
    bitmap = await createImageBitmap(file);
    if (version !== coverVersion) return;
    paint(art, bitmap, 768);
    paint(glow, art, 256);
    glowVersion++;
    art.hidden = false;
    artNote.hidden = true;
    main.setAttribute('data-glow', '');
  } catch {
    // Preview is optional. The server still checks the original File on confirmation.
    if (version !== coverVersion) return;
    art.width = art.height = 1;
    fadeGlow();
    artNote.textContent = 'Preview unavailable';
    coverName.textContent = `${file.name} — preview unavailable. MusicLib checks the file when you confirm.`;
  } finally { bitmap?.close(); }
}
function selectCover(file) {
  if (fields.disabled) return;
  // These are picker hints, not content validation. A supported suffix
  // is enough even when the OS has supplied no (or an unexpected) MIME type.
  if (file && (!file.size || !(/\.(jpe?g|png)$/i.test(file.name) || /^image\/(jpeg|png)$/.test(file.type)))) {
    return coverNotice('Choose a non-empty JPEG or PNG file.');
  }
  dirty();
  coverFile = file;
  const version = ++coverVersion;
  coverName.textContent = file ? file.name : '';
  chooseCover.textContent = file ? 'Change cover' : 'Choose cover';
  clear.hidden = !file;
  coverError.hidden = true;
  art.hidden = true;
  artNote.hidden = false;
  artNote.textContent = file ? 'Preparing preview…' : 'Drop one JPEG or PNG';
  art.width = art.height = 1;
  // Leave the background alone until the winning decode paints or fails.
  if (file) previewCover(file, version);
  else fadeGlow();
}
chooseCover.onclick = () => cover.click();
cover.onchange = () => {
  const [file] = cover.files;
  cover.value = '';
  if (file) selectCover(file);
};
clear.onclick = () => { selectCover(null); chooseCover.focus(); };
form.addEventListener('invalid', ({ target }) => target.closest('details')?.setAttribute('open', ''), true);

function values(root) {
  const out = {};
  for (const f of root.querySelectorAll('input:not([type=checkbox])')) {
    if (!f.disabled && f.value !== '') out[f.name] = f.type === 'number' ? f.valueAsNumber : f.value;
  }
  return out;
}
function body() {
  const parts = [];
  function declare(file) {
    const part_id = `file_${parts.length}`;
    parts.push([part_id, file]);
    return { part_id, name: file.name, size: file.size };
  }
  const metadata = { creation_id: id, ...values($('#draft-album')), compilation: form.elements.compilation.checked };
  if (choice.value === 'name') metadata.artist_name = name.value;
  else metadata.artist_id = choice.value;
  metadata.tracks = [...tracks.values()].map(({ file, row }) => {
    const track = { ...declare(file), ...values(row) };
    if ($('[name=nogenre]', row).checked) track.genre = '';
    return track;
  });
  if (coverFile) metadata.cover = declare(coverFile);
  const data = new FormData();
  data.append('metadata', JSON.stringify(metadata));
  for (const [part, file] of parts) data.append(part, file);
  return data;
}

function json(r) {
  if (/^application\/json(?:;|$)/i.test(r.headers.get('Content-Type'))) return r.json();
}
function openAlbum(notes) {
  done = true;
  try { sessionStorage.setItem('creation-warnings', JSON.stringify({ id, notes })); } catch { /* Optional. */ }
  removeEventListener('beforeunload', unload);
  location.replace(`/albums/${id}`);
}
async function confirmAlbum(body) {
  try {
    const r = await fetch('/api/albums', {
      method: 'POST', headers: { 'X-Musiclib-Request': '1' }, body, signal: request.signal, redirect: 'error'
    });
    const a = await json(r);
    if (request.signal.aborted) throw 0;
    if (r.status === 201 && r.headers.get('Location') === api && a?.id === id && Array.isArray(a.warnings)) {
      return openAlbum(a.warnings.map(w => w.message));
    }
    if (r.status === 409 && a?.code === 'creation_conflict' && a.details?.album_id === id) {
      done = true;
      $('#draft-existing').hidden = false;
      return notice('An album already exists for this draft. This submission was not applied.');
    }
    if ([400, 401, 403, 404, 409, 413, 415, 421, 422, 507].includes(r.status) && a?.details &&
        typeof a.message === 'string' && typeof a.code === 'string' &&
        !['creation_conflict', 'upload_incomplete'].includes(a.code)) return notice(a.message, a.details);
  } catch { /* A lost reply does not prove failure. */ }
  status.textContent = 'Checking whether the album was saved…';
  if (document.activeElement === stop) status.focus();
  stop.hidden = true;
  try {
    const r = await fetch(api, { cache: 'no-store', redirect: 'error', signal: AbortSignal.timeout(8000) });
    if (r.status === 200 && (await json(r))?.id === id) {
      return openAlbum(['Album found after an uncertain reply. Check the saved fields.']);
    }
  } catch { /* Lookup failed. */ }
  pending = true;
  notice('MusicLib couldn’t confirm the save. Retry this draft to use the same album ID, without creating a second album.');
}

stop.onclick = () => request.abort();
form.onsubmit = async e => {
  e.preventDefault();
  if (submit.disabled) return;
  if (!tracks.size) return trackNotice('Add at least one audio file.');
  dirty();
  const data = body();
  fields.disabled = submit.disabled = error.hidden = true;
  showDrop();
  stop.hidden = false;
  request = new AbortController();
  submit.textContent = 'Creating album…';
  status.textContent = 'Uploading and checking audio. Keep this tab open; this may take a while.';
  status.focus();
  await confirmAlbum(data);
  stop.hidden = true;
  fields.disabled = submit.disabled = !!done;
  submit.textContent = pending && !done ? 'Retry this draft' : 'Create album';
  status.textContent = pending && !done ? 'A previous attempt may still be saving this album.' : '';
};
form.hidden = false;
