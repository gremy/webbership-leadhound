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
// The ticks are read afresh on each count, because the triage script below removes rows and puts
// them back; it fires "leads:rows" when it does.
(function () {
  "use strict";
  var form = document.getElementById("send-now-form");
  if (!form) {
    return;
  }
  var all = document.querySelector("input[data-send-all]");
  var count = form.querySelector("[data-send-count]");
  var said = form.querySelector("[data-send-selected]");
  var idle = said ? said.textContent : "";
  function update() {
    var chosen = {};
    var n = 0;
    document.querySelectorAll("input[data-send-pick]").forEach(function (p) {
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
  document.addEventListener("change", function (e) {
    if (e.target.matches && e.target.matches("input[data-send-pick]")) {
      update();
    }
  });
  document.addEventListener("leads:rows", update);
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

// The Leads list's triage. A review action (a form with data-triage) is posted by fetch with the
// header X-Triage: 1, so the page keeps its place: on 204 the row leaves both layouts (or, for
// Good, stays marked), the Send now count is redone, focus goes to the next row, and a toast says
// what happened, with Undo for the last skip. On a refusal the row stays and the toast shows the
// server's words. Only when the request never reached the server is the form posted the plain
// way; never after an answer, which would post twice. Keys: j and k move, e opens More, s skips,
// g says Good, r switches roastery and coffee shop, x ticks for Send now, u undoes the last skip;
// none of them acts while typing, with a modifier held, or on a page with no rows. Without this
// file every form posts as it is and comes back to the next row.
(function () {
  "use strict";
  var list = document.querySelector("[data-triage-list]");
  if (!list || !window.fetch || !window.URLSearchParams) {
    return;
  }
  var placeFiltered = list.hasAttribute("data-place-filtered");
  var TOAST_MS = 8000;
  var toast = null;
  var toastTimer = 0;
  var lastSkip = null; // { id, name, back, removed: [{ node, parent, index }] }; kept for one action
  var current = null; // the current row of the visible layout
  var busy = false;

  // The rows of the layout on screen: each lead's tbody in the table, each card in the list.
  function rows() {
    var table = list.querySelector(".wide-only");
    var wide = table && table.offsetParent !== null;
    return Array.prototype.slice.call(list.querySelectorAll(wide ? "tbody.lead-group" : "li.lead-item-narrow"));
  }
  function rowOf(el) {
    return el.closest("tbody.lead-group, li.lead-item-narrow");
  }
  function idOf(row) {
    var el = row.id ? row : row.querySelector("tr[id]");
    return el ? el.id.replace(/^lead-[nw]-/, "") : "";
  }
  // Both layouts' nodes of lead id, those that are on the page.
  function nodesOf(id) {
    var out = [];
    var n = document.getElementById("lead-n-" + id);
    var w = document.getElementById("lead-w-" + id);
    if (n) {
      out.push(n);
    }
    if (w && w.parentNode) {
      out.push(w.parentNode);
    }
    return out;
  }

  function setCurrent(row, focus) {
    if (current && current !== row) {
      current.classList.remove("is-current");
    }
    current = row;
    if (!row) {
      return;
    }
    row.classList.add("is-current");
    if (focus) {
      row.tabIndex = -1;
      row.focus({ preventScroll: true });
      row.scrollIntoView({ block: "nearest" });
    }
  }
  function moveBy(step) {
    var all = rows();
    if (!all.length) {
      return;
    }
    var i = all.indexOf(current);
    i = i < 0 ? 0 : Math.min(all.length - 1, Math.max(0, i + step));
    setCurrent(all[i], true);
  }

  function say(words, isError, undo) {
    if (!toast) {
      toast = document.createElement("div");
      toast.className = "toast";
      toast.setAttribute("role", "status");
      toast.setAttribute("aria-live", "polite");
      document.body.appendChild(toast);
    }
    toast.textContent = "";
    toast.classList.toggle("toast-error", !!isError);
    var span = document.createElement("span");
    span.textContent = words;
    toast.appendChild(span);
    if (undo) {
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn btn-secondary btn-sm";
      btn.textContent = "Undo";
      btn.addEventListener("click", undoSkip);
      toast.appendChild(btn);
    }
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      toast.hidden = true;
    }, TOAST_MS);
  }

  // Takes lead id's rows off the page, both layouts, and says where each stood.
  function removeRows(id) {
    return nodesOf(id).map(function (node) {
      var parent = node.parentNode;
      var index = Array.prototype.indexOf.call(parent.children, node);
      parent.removeChild(node);
      return { node: node, parent: parent, index: index };
    });
  }
  // After a row left: the row now at the same place becomes current, or the one before it;
  // with none left, the list says so and offers the page afresh.
  function afterRemoval(at) {
    var all = rows();
    if (all.length) {
      setCurrent(all[Math.min(at, all.length - 1)], true);
    } else {
      current = null;
      if (!list.querySelector("[data-triage-done]")) {
        var p = document.createElement("p");
        p.className = "empty-text";
        p.setAttribute("data-triage-done", "");
        var a = document.createElement("a");
        a.href = window.location.pathname + window.location.search;
        a.textContent = "Show the next leads";
        p.appendChild(document.createTextNode("Every lead on this page is reviewed. "));
        p.appendChild(a);
        list.appendChild(p);
      }
    }
    document.dispatchEvent(new Event("leads:rows"));
  }

  function send(action, back) {
    return fetch(action, {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-Triage": "1" },
      body: new URLSearchParams({ back: back }),
    });
  }
  // A refused or unexpected answer: a 422's own words, anything else a plain line. Either way
  // the server answered, so nothing is posted again.
  function refused(res) {
    if (res.status === 422) {
      return res.text().then(function (t) {
        say(t.trim() || "That was refused; nothing changed.", true);
      });
    }
    say("That did not go through (" + res.status + "). Reload the page and try again.", true);
  }

  function post(form) {
    if (busy) {
      return;
    }
    var box = form.closest(".lead-triage");
    var verb = form.getAttribute("data-triage");
    var id = box ? box.getAttribute("data-lead") : "";
    var name = box ? box.getAttribute("data-name") : "";
    var back = form.elements.back ? form.elements.back.value : "";
    var row = rowOf(form);
    if (row && rows().indexOf(row) >= 0) {
      setCurrent(row, false);
    }
    busy = true;
    send(form.action, back).then(function (res) {
      busy = false;
      if (res.status !== 204) {
        return refused(res);
      }
      done(verb, id, name, back);
    }, function () {
      busy = false;
      form.submit();
    });
  }

  function done(verb, id, name, back) {
    var at = Math.max(0, rows().indexOf(current));
    if (verb === "skip") {
      lastSkip = { id: id, name: name, back: back, removed: removeRows(id) };
      say("Skipped " + name + ".", false, true);
      afterRemoval(at);
      return;
    }
    lastSkip = null;
    if (verb === "good") {
      nodesOf(id).forEach(function (node) {
        node.querySelectorAll('form[data-triage="good"]').forEach(function (f) {
          var badge = document.createElement("span");
          badge.className = "badge badge-good";
          badge.textContent = "Good";
          f.parentNode.replaceChild(badge, f);
        });
      });
      say("Saved " + name + " as likely.");
      moveBy(1);
      return;
    }
    if ((verb === "roastery" || verb === "cafe") && placeFiltered) {
      removeRows(id);
      say(verb === "roastery" ? "Tagged " + name + " a roastery; it is under Roasteries now." :
        "Tagged " + name + " a coffee shop; it is under Coffee shops now.");
      afterRemoval(at);
      return;
    }
    // The row's own Undo, or a switch on a list that shows both place types: the row changes
    // in place, which the server words best.
    window.location.reload();
  }

  function undoSkip() {
    var s = lastSkip;
    if (!s || busy) {
      return;
    }
    busy = true;
    send("/leads/" + encodeURIComponent(s.id) + "/unskip", s.back).then(function (res) {
      busy = false;
      if (res.status !== 204) {
        return refused(res);
      }
      lastSkip = null;
      s.removed.forEach(function (r) {
        r.parent.insertBefore(r.node, r.parent.children[r.index] || null);
      });
      var note = list.querySelector("[data-triage-done]");
      if (note) {
        note.parentNode.removeChild(note);
      }
      say(s.name + " is back in the list.");
      var back = rows().filter(function (row) {
        return idOf(row) === s.id;
      })[0];
      setCurrent(back || null, true);
      document.dispatchEvent(new Event("leads:rows"));
    }, function () {
      busy = false;
      say("Undo did not reach the server; the lead is still skipped.", true);
    });
  }

  list.addEventListener("submit", function (e) {
    var form = e.target.closest("form[data-triage]");
    if (form) {
      e.preventDefault();
      post(form);
    }
  });
  list.addEventListener("focusin", function (e) {
    var row = rowOf(e.target);
    if (row && row !== current && rows().indexOf(row) >= 0) {
      setCurrent(row, false);
    }
  });

  function act(selector) {
    var all = rows();
    if (!current || all.indexOf(current) < 0) {
      setCurrent(all[0], true);
    }
    var form = current.querySelector(selector);
    if (form) {
      post(form);
    }
  }
  var keys = {
    j: function () {
      moveBy(1);
    },
    k: function () {
      moveBy(-1);
    },
    e: function () {
      if (!current) {
        moveBy(0);
      }
      var d = current && current.querySelector("details.lead-more");
      if (d) {
        d.open = !d.open;
      }
    },
    s: function () {
      act('form[data-triage="skip"]');
    },
    g: function () {
      act('form[data-triage="good"]');
    },
    r: function () {
      act('form[data-triage="roastery"], form[data-triage="cafe"]');
    },
    x: function () {
      if (!current) {
        moveBy(0);
      }
      var tick = current && current.querySelector("input[data-send-pick]");
      if (tick) {
        tick.click();
      }
    },
    u: undoSkip,
  };
  document.addEventListener("keydown", function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey || !keys.hasOwnProperty(e.key)) {
      return;
    }
    var t = e.target;
    if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) {
      return;
    }
    if (!rows().length) {
      return;
    }
    e.preventDefault();
    keys[e.key]();
  });

  var hint = list.querySelector("[data-key-hint]");
  if (hint && rows().length) {
    hint.hidden = false;
  }
})();
