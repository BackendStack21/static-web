/* ===========================
   Particles Canvas Animation
   — ResizeObserver + running flag for proper lifecycle
   =========================== */
(function initParticles() {
  const canvas = document.getElementById('particles-canvas');
  if (!canvas) return;

  const ctx = canvas.getContext('2d');
  let W = 0;
  let H = 0;
  let particles = [];
  let animId = null;
  let running = false;

  const PARTICLE_COUNT = 55;
  const CONNECTION_DIST = 140;
  const COLORS = ['#6366f1', '#8b5cf6', '#22d3ee', '#34d399', '#a855f7'];

  function randomColor() {
    return COLORS[Math.floor(Math.random() * COLORS.length)];
  }

  function resize() {
    const hero = canvas.parentElement;
    if (!hero) return;
    W = canvas.width = hero.offsetWidth;
    H = canvas.height = hero.offsetHeight;
  }

  function createParticles() {
    particles = [];
    for (let i = 0; i < PARTICLE_COUNT; i++) {
      particles.push({
        x: Math.random() * W,
        y: Math.random() * H,
        vx: (Math.random() - 0.5) * 0.55,
        vy: (Math.random() - 0.5) * 0.55,
        r: Math.random() * 2 + 1.2,
        color: randomColor(),
        opacity: Math.random() * 0.5 + 0.2,
      });
    }
  }

  function drawFrame() {
    if (!running) return;

    ctx.clearRect(0, 0, W, H);

    // Draw connections
    for (let i = 0; i < particles.length; i++) {
      for (let j = i + 1; j < particles.length; j++) {
        const dx = particles[i].x - particles[j].x;
        const dy = particles[i].y - particles[j].y;
        const dist = Math.sqrt(dx * dx + dy * dy);
        if (dist < CONNECTION_DIST) {
          const alpha = (1 - dist / CONNECTION_DIST) * 0.18;
          ctx.beginPath();
          ctx.strokeStyle = 'rgba(99,102,241,' + alpha + ')';
          ctx.lineWidth = 0.8;
          ctx.moveTo(particles[i].x, particles[i].y);
          ctx.lineTo(particles[j].x, particles[j].y);
          ctx.stroke();
        }
      }
    }

    // Draw & move particles
    for (const p of particles) {
      ctx.beginPath();
      ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2);
      ctx.fillStyle = p.color;
      ctx.globalAlpha = p.opacity;
      ctx.fill();
      ctx.globalAlpha = 1;

      p.x += p.vx;
      p.y += p.vy;

      if (p.x < 0 || p.x > W) p.vx *= -1;
      if (p.y < 0 || p.y > H) p.vy *= -1;
    }

    animId = requestAnimationFrame(drawFrame);
  }

  function start() {
    if (running) return;
    running = true;
    drawFrame();
  }

  function stop() {
    running = false;
    if (animId) {
      cancelAnimationFrame(animId);
      animId = null;
    }
  }

  // ResizeObserver for proper canvas sizing (fixes zero-height race condition)
  const ro = new ResizeObserver(function () {
    resize();
    if (particles.length === 0 || W === 0 || H === 0) return;
    // Reclamp particles to new bounds
    for (const p of particles) {
      if (p.x > W) p.x = Math.random() * W;
      if (p.y > H) p.y = Math.random() * H;
    }
  });
  ro.observe(canvas.parentElement);

  // IntersectionObserver — pause when hero scrolls out of view
  const io = new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (e.isIntersecting) {
        start();
      } else {
        stop();
      }
    });
  });
  io.observe(canvas.parentElement);

  // Initial setup
  resize();
  createParticles();
  start();
})();

/* ===========================
   Hamburger Menu Toggle
   =========================== */
(function initHamburger() {
  const btn = document.querySelector('.nav-hamburger');
  const nav = document.querySelector('.nav');
  if (!btn || !nav) return;

  function openMenu() {
    nav.classList.add('nav-open');
    btn.setAttribute('aria-expanded', 'true');
    document.body.style.overflow = 'hidden';
  }

  function closeMenu() {
    nav.classList.remove('nav-open');
    btn.setAttribute('aria-expanded', 'false');
    document.body.style.overflow = '';
  }

  function toggle() {
    if (nav.classList.contains('nav-open')) {
      closeMenu();
    } else {
      openMenu();
    }
  }

  btn.addEventListener('click', toggle);

  // Close on nav link click
  document.querySelectorAll('.nav-links a').forEach(function (link) {
    link.addEventListener('click', closeMenu);
  });

  // Close on Escape
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && nav.classList.contains('nav-open')) {
      closeMenu();
      btn.focus();
    }
  });
})();

/* ===========================
   Tab Switching with ARIA
   — Arrow key navigation, hidden attribute, aria-selected
   =========================== */
(function initTabs() {
  var tablists = document.querySelectorAll('[role="tablist"]');

  tablists.forEach(function (tablist) {
    var tabs = Array.from(tablist.querySelectorAll('[role="tab"]'));
    var container = tablist.closest('.code-tabs') || tablist.closest('.security-tabs') || tablist.closest('.config-tabs');
    if (!container || tabs.length === 0) return;

    function activateTab(tab) {
      // Deactivate all
      tabs.forEach(function (t) {
        t.classList.remove('active');
        t.setAttribute('aria-selected', 'false');
        t.setAttribute('tabindex', '-1');
        var panelId = t.getAttribute('aria-controls');
        var panel = panelId ? container.querySelector('#' + panelId) : null;
        if (panel) {
          panel.classList.remove('active');
          panel.setAttribute('hidden', '');
        }
      });

      // Activate target
      tab.classList.add('active');
      tab.setAttribute('aria-selected', 'true');
      tab.setAttribute('tabindex', '0');
      var panelId = tab.getAttribute('aria-controls');
      var panel = panelId ? container.querySelector('#' + panelId) : null;
      if (panel) {
        panel.classList.add('active');
        panel.removeAttribute('hidden');
      }
    }

    // Click handler
    tabs.forEach(function (tab) {
      tab.addEventListener('click', function () {
        activateTab(tab);
      });
    });

    // Arrow key navigation (left/right)
    tablist.addEventListener('keydown', function (e) {
      var idx = tabs.indexOf(document.activeElement);
      if (idx === -1) return;

      var next = -1;
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
        next = (idx + 1) % tabs.length;
      } else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
        next = (idx - 1 + tabs.length) % tabs.length;
      } else if (e.key === 'Home') {
        next = 0;
      } else if (e.key === 'End') {
        next = tabs.length - 1;
      }

      if (next !== -1) {
        e.preventDefault();
        tabs[next].focus();
        activateTab(tabs[next]);
      }
    });

    // Set initial tabindex
    tabs.forEach(function (tab, i) {
      tab.setAttribute('tabindex', tab.getAttribute('aria-selected') === 'true' ? '0' : '-1');
    });
  });
})();

/* ===========================
   Copy Buttons — Event Delegation
   — data-copy: literal text to copy
   — data-copy-from: id of element whose textContent to copy
   =========================== */
(function initCopy() {
  var checkSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="#34d399" stroke-width="2.5" aria-hidden="true"><polyline points="20 6 9 17 4 12"></polyline></svg>';

  function showSuccess(btn) {
    var original = btn.innerHTML;
    btn.innerHTML = checkSvg;
    btn.style.color = '#34d399';
    setTimeout(function () {
      btn.innerHTML = original;
      btn.style.color = '';
    }, 1800);
  }

  function doCopy(text, btn) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () {
        showSuccess(btn);
      }).catch(function () {
        fallbackCopy(text, btn);
      });
    } else {
      fallbackCopy(text, btn);
    }
  }

  function fallbackCopy(text, btn) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand('copy');
      showSuccess(btn);
    } catch (_) {
      // silently fail
    }
    document.body.removeChild(ta);
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy], [data-copy-from]');
    if (!btn) return;

    var text = '';
    if (btn.hasAttribute('data-copy')) {
      text = btn.getAttribute('data-copy');
    } else if (btn.hasAttribute('data-copy-from')) {
      var source = document.getElementById(btn.getAttribute('data-copy-from'));
      if (source) {
        text = (source.innerText || source.textContent || '').trim();
      }
    }

    if (text) {
      doCopy(text, btn);
    }
  });
})();

/* ===========================
   Smooth scroll for nav links
   =========================== */
document.querySelectorAll('a[href^="#"]').forEach(function (link) {
  link.addEventListener('click', function (e) {
    var target = document.querySelector(link.getAttribute('href'));
    if (target) {
      e.preventDefault();
      target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  });
});

/* ===========================
   Scroll-Reveal with Stagger
   — Groups elements by parent section for incremental delays
   =========================== */
(function initReveal() {
  var selectors = '.feature-card, .pipeline-step, .perf-card';
  var targets = document.querySelectorAll(selectors);
  if (!targets.length) return;

  // Group by closest section-full or section
  var groups = new Map();
  targets.forEach(function (el) {
    var section = el.closest('.section-full') || el.closest('section');
    var key = section || document.body;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(el);
  });

  // Add reveal class and stagger delays per group
  var STAGGER_MS = 80;
  groups.forEach(function (elements) {
    elements.forEach(function (el, i) {
      el.classList.add('reveal');
      el.style.transitionDelay = (i * STAGGER_MS) + 'ms';
    });
  });

  var io = new IntersectionObserver(
    function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          e.target.classList.add('visible');
          io.unobserve(e.target);
        }
      });
    },
    { threshold: 0.1, rootMargin: '0px 0px -40px 0px' }
  );

  targets.forEach(function (el) { io.observe(el); });
})();
