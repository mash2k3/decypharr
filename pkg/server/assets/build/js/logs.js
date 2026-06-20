(function () {
    const API = (window.API || '/api').replace(/\/$/, '');
    const viewer = document.getElementById('log-viewer');
    const levelSelect = document.getElementById('level-select');
    const linesSelect = document.getElementById('lines-select');
    const filterInput = document.getElementById('filter-input');
    const chipBar = document.getElementById('chip-bar');
    const statusEl = document.getElementById('log-status');
    const scrollBtn = document.getElementById('scroll-bottom-btn');
    const refreshBtn = document.getElementById('refresh-btn');
    const autoScrollToggle = document.getElementById('auto-scroll-toggle');

    let allLines = [];           // raw log lines from last fetch
    let activeChips = new Set(); // component filters; empty = show all
    let knownComponents = new Set();
    let pollTimer = null;
    let isPaused = false;

    // --- Fetch ---
    async function fetchLogs() {
        const lines = linesSelect.value || '2000';
        const level = levelSelect.value || 'all';
        try {
            const res = await fetch(`${API}/logs?lines=${lines}&level=${encodeURIComponent(level)}`, { credentials: 'same-origin' });
            if (!res.ok) throw new Error(`HTTP ${res.status}`);
            const data = await res.json();
            allLines = data.lines || [];
            extractComponents(allLines);
            renderLines();
            setStatus(`${allLines.length} lines`);
        } catch (e) {
            setStatus('Error: ' + e.message);
        }
    }

    // --- Component chip extraction ---
    function extractComponents(lines) {
        const prev = new Set(knownComponents);
        lines.forEach(line => {
            const m = line.match(/\[([a-zA-Z0-9_\-]+)\]/);
            if (m) knownComponents.add(m[1].toLowerCase());
        });
        // Only re-render chips if new ones appeared
        let changed = false;
        knownComponents.forEach(c => { if (!prev.has(c)) changed = true; });
        if (changed) renderChips();
    }

    function renderChips() {
        chipBar.innerHTML = '';
        const sorted = [...knownComponents].sort();
        sorted.forEach(comp => {
            const btn = document.createElement('button');
            btn.className = 'btn btn-xs btn-outline' + (activeChips.has(comp) ? ' chip-active' : '');
            btn.textContent = comp;
            btn.dataset.comp = comp;
            btn.addEventListener('click', () => {
                if (activeChips.has(comp)) activeChips.delete(comp);
                else activeChips.add(comp);
                btn.classList.toggle('chip-active', activeChips.has(comp));
                renderLines();
            });
            chipBar.appendChild(btn);
        });
    }

    // --- Render ---
    function renderLines() {
        const filterText = filterInput.value.toLowerCase();
        const frag = document.createDocumentFragment();
        let count = 0;

        allLines.forEach(raw => {
            // Component filter
            if (activeChips.size > 0) {
                const m = raw.match(/\[([a-zA-Z0-9_\-]+)\]/);
                const comp = m ? m[1].toLowerCase() : '';
                if (!activeChips.has(comp)) return;
            }
            // Text filter
            if (filterText && !raw.toLowerCase().includes(filterText)) return;

            const span = document.createElement('span');
            span.className = 'log-line';
            span.innerHTML = formatLine(raw);
            frag.appendChild(span);
            count++;
        });

        const atBottom = isAtBottom();
        viewer.innerHTML = '';
        viewer.appendChild(frag);
        setStatus(`${count} / ${allLines.length} lines`);

        if (autoScrollToggle.checked && (atBottom || !isPaused)) {
            scrollToBottom();
        }
    }

    // --- Line formatting ---
    // File log format: "2006-01-02 15:04:05 | LEVEL  | [component] message"
    function formatLine(raw) {
        const e = escHtml(raw);
        // Match: timestamp | LEVEL  | [component] rest
        const m = raw.match(/^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\s*\|\s*(\w+)\s*\|\s*(\[[^\]]+\])(.*)$/);
        if (!m) return `<span class="log-msg">${e}</span>`;
        const [, ts, level, comp, msg] = m;
        const lvlClass = levelClass(level.trim().toLowerCase());
        return `<span class="log-ts">${escHtml(ts)}</span> ` +
            `<span class="${lvlClass}">| ${escHtml(level.padEnd(6))} |</span> ` +
            `<span class="log-component">${escHtml(comp)}</span>` +
            `<span class="log-msg">${escHtml(msg)}</span>`;
    }

    function levelClass(level) {
        switch (level) {
            case 'info':  return 'log-level-info';
            case 'warn':  return 'log-level-warn';
            case 'error': return 'log-level-error';
            case 'debug': return 'log-level-debug';
            case 'fatal': return 'log-level-fatal';
            default:      return 'log-msg';
        }
    }

    function escHtml(s) {
        return String(s)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;');
    }

    // --- Scroll helpers ---
    function isAtBottom() {
        return viewer.scrollHeight - viewer.scrollTop - viewer.clientHeight < 40;
    }

    function scrollToBottom() {
        viewer.scrollTop = viewer.scrollHeight;
    }

    viewer.addEventListener('scroll', () => {
        const atBottom = isAtBottom();
        isPaused = !atBottom;
        scrollBtn.style.display = atBottom ? 'none' : '';
    });

    scrollBtn.addEventListener('click', () => {
        autoScrollToggle.checked = true;
        isPaused = false;
        scrollToBottom();
        scrollBtn.style.display = 'none';
    });

    // --- Status ---
    function setStatus(msg) {
        if (statusEl) statusEl.textContent = msg;
    }

    // --- Auto-poll every 3 seconds ---
    function startPoll() {
        if (pollTimer) clearInterval(pollTimer);
        pollTimer = setInterval(fetchLogs, 3000);
    }

    // --- Event bindings ---
    levelSelect.addEventListener('change', fetchLogs);
    linesSelect.addEventListener('change', fetchLogs);
    filterInput.addEventListener('input', () => renderLines());
    refreshBtn.addEventListener('click', () => {
        refreshBtn.disabled = true;
        fetchLogs().finally(() => { refreshBtn.disabled = false; });
    });

    // Initial load + start polling
    fetchLogs();
    startPoll();

    // Pause polling when tab hidden, resume on visible
    document.addEventListener('visibilitychange', () => {
        if (document.hidden) {
            clearInterval(pollTimer);
            pollTimer = null;
        } else {
            fetchLogs();
            startPoll();
        }
    });
})();
