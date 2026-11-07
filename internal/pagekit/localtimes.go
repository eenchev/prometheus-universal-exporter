package pagekit

// The pages write a moment one way: as a <time datetime="…"> of RFC 3339
// in UTC, which a page without JavaScript shows as it is. LocalTimesScript
// defines localTimes(root), which a page's script calls on its content,
// after each refresh too, so every moment reads alike:
//
//   - a <time> whose text is the moment is shown in the browser's time, the
//     time of day alone when it is today and the date with it otherwise,
//     its tooltip the moment in UTC and how long ago it is, or how long
//     until it, from now;
//   - a <time class="ago">, whose text says how long ago the moment was, as
//     the server wrote it, keeps its text, its tooltip the moment in the
//     browser's time and in UTC, after its data-note when it has one;
//   - an element with data-at, a strip's mark, has the moment that begins
//     its tooltip written in the browser's time, UTC after it.
//
// How long ago is written as the pages write a duration (Duration): whole
// seconds, minutes and hours, 1h2m, 0s for none.
//
// It is text for a page's own script, inside the function that script runs
// in, so it adds nothing to the page's global names.
const LocalTimesScript = `
  function localTimes(root) {
    const now = Date.now(), today = new Date(now).toDateString();
    const local = (ms) => {
      const d = new Date(ms);
      return d.toDateString() === today ? d.toLocaleTimeString() : d.toLocaleString();
    };
    const utc = (ms) => new Date(ms).toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC");
    const span = (ms) => {
      let s = Math.round(Math.abs(ms) / 1000), out = "";
      const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
      s %= 60;
      if (h) out += h + "h";
      if (m) out += m + "m";
      if (s || !out) out += s + "s";
      return out;
    };
    const relative = (ms) => ms <= now ? span(now - ms) + " ago" : "in " + span(ms - now);
    for (const t of root.querySelectorAll("time[datetime]")) {
      const at = t.getAttribute("datetime"), ms = Date.parse(at);
      if (Number.isNaN(ms) || t.closest("#as-of")) continue;
      if (t.classList.contains("ago")) {
        t.title = (t.dataset.note ? t.dataset.note + "; " : "") + local(ms) + " (" + utc(ms) + ")";
      } else {
        t.textContent = local(ms);
        t.title = utc(ms) + ", " + relative(ms);
      }
    }
    for (const mark of root.querySelectorAll("[data-at]")) {
      const at = mark.dataset.at, ms = Date.parse(at);
      if (Number.isNaN(ms) || !mark.title.startsWith(at)) continue;
      mark.title = local(ms) + " (" + utc(ms) + ")" + mark.title.slice(at.length);
    }
  }
`
