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
