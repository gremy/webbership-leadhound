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
