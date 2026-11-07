package pagekit

// SortableScript defines sortable(content, tableID, filterID, noun), which
// makes the table of tableID in the element content sortable and its rows
// filtered by the search box of filterID, and returns the function that
// arranges the table again, for the page to call after it swaps new
// content in. It is text for a page's own script, inside the function that
// script runs in, so it adds nothing to the page's global names.
//
// The table sorts by the column whose heading is clicked, the other way at
// a second click: numbers, a heading with data-sort="num", by each cell's
// data-value, and text as it reads, the numbers in it by their value, job2
// before job10, a letter's case no matter; a tie, and the order before any
// click, is the rows' data-order. Its rows are filtered as the box is
// typed in, matching in any case the text each row carries in data-match,
// and the element with data-rows-of="<tableID>" says how many of the noun
// (plural) match. The headings are made buttons here, the table marked
// sortable, with aria-sort on each heading: without JavaScript they are
// plain headings, as nothing would sort the table, and the filter's form,
// hidden, is shown. The table is in the content, which a refresh
// replaces, so the order chosen and the text typed are kept here and put
// back on the new table, the box keeping focus and its caret where it was.
const SortableScript = SortOrderScript + `
  function sortable(content, tableID, filterID, noun) {
    let column = -1, down = false, typed = "";
    function arrange(typing) {
      const table = content.querySelector("table#" + tableID), box = document.getElementById(filterID);
      if (!table) return;
      if (box) {
        box.value = typed;
        box.form.hidden = false;
        if (typing) {
          box.focus();
          box.setSelectionRange(typing[0], typing[1]);
        }
      }
      const heads = table.tHead.rows[table.tHead.rows.length - 1].cells, body = table.tBodies[0];
      if (!table.classList.contains("sortable")) {
        for (const head of heads) {
          const button = document.createElement("button");
          button.type = "button";
          button.append(...head.childNodes);
          head.append(button);
        }
        table.classList.add("sortable");
      }
      const numeric = column >= 0 && heads[column].dataset.sort === "num";
      const key = (row) => {
        const cell = row.cells[column];
        return numeric ? Number(cell.dataset.value) : cell.textContent.trim();
      };
      const rows = Array.from(body.rows).sort((x, y) => {
        let c = 0;
        if (column >= 0) c = numeric ? key(x) - key(y) : textOrder(key(x), key(y));
        if (down) c = -c;
        return c || Number(x.dataset.order) - Number(y.dataset.order);
      });
      for (const row of rows) body.appendChild(row);
      markSorted(heads, heads[column], down);
      const needle = typed.trim().toLowerCase();
      let shown = 0;
      for (const row of rows) {
        row.hidden = needle !== "" && !(row.dataset.match || "").toLowerCase().includes(needle);
        if (!row.hidden) shown++;
      }
      const count = content.querySelector('[data-rows-of="' + tableID + '"]');
      if (count) count.textContent = needle === "" ? "" : shown === 0 ? "None of the " + noun + " holds what is typed in the filter." : shown + " of the " + rows.length + " " + noun + " match.";
    }
    content.addEventListener("click", (event) => {
      const button = event.target.closest("table#" + tableID + " th button");
      if (!button) return;
      const at = button.parentElement.cellIndex;
      down = at === column ? !down : false;
      column = at;
      arrange();
      button.focus();
    });
    content.addEventListener("input", (event) => {
      if (event.target.id !== filterID) return;
      typed = event.target.value;
      arrange();
    });
    content.addEventListener("submit", (event) => {
      if (event.target.contains(document.getElementById(filterID))) event.preventDefault();
    });
    arrange();
    // The page calls this after it swaps new content in.
    return (swapIn) => {
      const box = document.getElementById(filterID);
      const typing = box && document.activeElement === box ? [box.selectionStart, box.selectionEnd] : null;
      swapIn();
      arrange(typing);
    };
  }
`

// SortOrderScript defines what every sortable table of the pages sorts and
// marks by, SortableScript's, whose rows are in the page, and the debug
// page's series tables, whose rows are drawn from an answer's data, the
// first of them alone: textOrder(x, y), which orders text as it reads, the
// numbers in it by their value, job2 before job10, a letter's case no
// matter; and markSorted(heads, sorted, down), which sets aria-sort on each
// heading of heads, ascending or descending on sorted, the heading sorted
// by (null for none), and none on the others, as SortableStyle marks them.
// It is text for a page's own script, as SortableScript is.
const SortOrderScript = `
  const textOrder = new Intl.Collator(undefined, {numeric: true, sensitivity: "base"}).compare;
  function markSorted(heads, sorted, down) {
    for (const head of heads) head.setAttribute("aria-sort", head === sorted ? (down ? "descending" : "ascending") : "none");
  }
`

// SortableStyle styles a table SortableScript made sortable and its filter
// form, form.filter.table. Each heading's button shows the order it is
// sorted by: ↕ for a column not sorted by, ↑ or ↓ for the one sorted by. A
// number's column (th.num) has the mark before its name, so the name ends
// where the column's numbers end. The space between a mark and the name is
// \00a0, a no-break space: a space after an escape such as \2195 ends the
// escape and is no part of the text, so "\2195 " is the mark alone, flush
// with the name.
const SortableStyle = `
table.sortable th button { padding: 0; border: 0; background: transparent; color: var(--fg); font: inherit; font-weight: 600; cursor: pointer; white-space: nowrap; }
table.sortable th:not(.num) button::after { content: "\00a0\2195"; color: var(--muted); }
table.sortable th.num button::before { content: "\2195\00a0"; color: var(--muted); }
table.sortable th[aria-sort="ascending"]:not(.num) button::after { content: "\00a0\2191"; color: var(--accent); }
table.sortable th[aria-sort="descending"]:not(.num) button::after { content: "\00a0\2193"; color: var(--accent); }
table.sortable th.num[aria-sort="ascending"] button::before { content: "\2191\00a0"; color: var(--accent); }
table.sortable th.num[aria-sort="descending"] button::before { content: "\2193\00a0"; color: var(--accent); }
form.filter.table { display: flex; flex-direction: column; gap: 4px; margin: 0 0 8px; max-width: 640px; }
form.filter.table[hidden] { display: none; }
form.filter.table label { font-size: 14px; font-weight: 600; }
form.filter.table input[type="search"] { width: 100%; box-sizing: border-box; height: 34px; padding: 0 10px; font: inherit; color: var(--fg); background: var(--bg); border: 1px solid var(--field); border-radius: 6px; }
`
