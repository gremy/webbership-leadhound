// LeadHound console: small comforts only; every page works without this file.
// The gear menu is a <details>: it closes on a click outside it and on Escape.
(function () {
  "use strict";
  function openMenus() {
    return document.querySelectorAll("details.gear[open]");
  }
  document.addEventListener("click", function (e) {
    openMenus().forEach(function (d) {
      if (!d.contains(e.target)) {
        d.open = false;
      }
    });
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") {
      return;
    }
    openMenus().forEach(function (d) {
      d.open = false;
      var s = d.querySelector("summary");
      if (s) {
        s.focus();
      }
    });
  });
})();

// The Leads list's Send now: count the chosen leads on the button and the bar, mark chosen rows,
// and offer "choose every lead on this page". Without this file the ticked leads post all the same.
(function () {
  "use strict";
  var form = document.getElementById("send-now-form");
  if (!form) {
    return;
  }
  var picks = document.querySelectorAll("input[data-send-pick]");
  var all = document.querySelector("input[data-send-all]");
  var count = form.querySelector("[data-send-count]");
  var said = form.querySelector("[data-send-selected]");
  var idle = said ? said.textContent : "";
  function update() {
    var chosen = {};
    var n = 0;
    picks.forEach(function (p) {
      var row = p.closest("tr");
      if (row) {
        row.classList.toggle("is-selected", p.checked);
      }
      if (p.checked && !chosen[p.value]) {
        chosen[p.value] = true;
        n++;
      }
    });
    if (count) {
      count.textContent = n ? " (" + n + ")" : "";
    }
    if (said) {
      said.textContent = n ? n + " selected" : idle;
    }
    if (all) {
      var wide = document.querySelectorAll("tr input[data-send-pick]");
      var on = 0;
      wide.forEach(function (p) {
        if (p.checked) {
          on++;
        }
      });
      all.checked = wide.length > 0 && on === wide.length;
      all.indeterminate = on > 0 && on < wide.length;
    }
  }
  picks.forEach(function (p) {
    p.addEventListener("change", update);
  });
  if (all) {
    all.hidden = false;
    all.addEventListener("change", function () {
      document.querySelectorAll("tr input[data-send-pick]").forEach(function (p) {
        p.checked = all.checked;
      });
      update();
    });
  }
  update();
})();

// The sub-nav scrolls sideways on a phone: bring the current page's link into view, so it is
// never hidden past the edge.
(function () {
  "use strict";
  var ul = document.querySelector(".subnav ul");
  var cur = ul && ul.querySelector("a[aria-current]");
  if (!cur || ul.scrollWidth <= ul.clientWidth) {
    return;
  }
  var left = cur.getBoundingClientRect().left - ul.getBoundingClientRect().left + ul.scrollLeft;
  ul.scrollLeft = Math.max(0, left - (ul.clientWidth - cur.offsetWidth) / 2);
})();

// A Copy button: <button type="button" data-copy="<id of a textarea>">. One listener on the
// document, so a button in a re-rendered fragment works too. The button says "Copied" only when
// the clipboard took the text; when it cannot, the text is selected for copying by hand.
(function () {
  "use strict";
  var COPIED_MS = 2000;
  var flashing = new WeakMap();
  function flash(btn) {
    var f = flashing.get(btn);
    if (f) {
      clearTimeout(f.timer);
    } else {
      f = { label: btn.textContent };
      flashing.set(btn, f);
    }
    btn.textContent = "Copied";
    f.timer = setTimeout(function () {
      btn.textContent = f.label;
      flashing.delete(btn);
    }, COPIED_MS);
  }
  function select(box) {
    box.focus();
    box.select();
    box.setSelectionRange(0, box.value.length);
  }
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    var box = btn && document.getElementById(btn.getAttribute("data-copy"));
    if (!box) {
      return;
    }
    if (!navigator.clipboard) {
      select(box);
      return;
    }
    navigator.clipboard.writeText(box.value).then(function () {
      flash(btn);
    }, function () {
      select(box);
    });
  });
})();
