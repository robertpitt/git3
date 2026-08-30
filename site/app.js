const menuButton = document.querySelector("[data-menu-button]");
const mobileMenu = document.querySelector("[data-mobile-menu]");

menuButton?.addEventListener("click", () => {
  const opening = mobileMenu?.classList.contains("hidden") ?? false;
  mobileMenu?.classList.toggle("hidden");
  menuButton.setAttribute("aria-expanded", String(opening));
});

mobileMenu?.querySelectorAll("a").forEach((link) => {
  link.addEventListener("click", () => {
    mobileMenu.classList.add("hidden");
    menuButton?.setAttribute("aria-expanded", "false");
  });
});

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }

  const input = document.createElement("textarea");
  input.value = text;
  input.setAttribute("readonly", "");
  input.style.position = "fixed";
  input.style.opacity = "0";
  document.body.append(input);
  input.select();
  document.execCommand("copy");
  input.remove();
}

const copyStatus = document.querySelector("[data-copy-status]");

document.querySelectorAll("[data-copy-target]").forEach((button) => {
  button.addEventListener("click", async () => {
    const target = document.getElementById(button.dataset.copyTarget);
    if (!target) return;

    const label = button.getAttribute("aria-label") ?? "Copy";

    try {
      await copyText(target.textContent.trim());
      button.setAttribute("aria-label", "Copied");
      button.classList.add("text-emerald-600");
      if (copyStatus) copyStatus.textContent = "Command copied to clipboard.";

      window.setTimeout(() => {
        button.setAttribute("aria-label", label);
        button.classList.remove("text-emerald-600");
      }, 1600);
    } catch {
      if (copyStatus) copyStatus.textContent = "Unable to copy the command.";
    }
  });
});

const releaseLabel = document.querySelector("[data-release-label]");

if (releaseLabel) {
  fetch("./release.json", { cache: "no-store" })
    .then((response) => {
      if (!response.ok) throw new Error("release metadata unavailable");
      return response.json();
    })
    .then(({ version }) => {
      if (typeof version === "string" && /^v\d+\.\d+\.\d+$/.test(version)) {
        releaseLabel.textContent = `${version} available`;
      }
    })
    .catch(() => {
      // Local previews intentionally keep the static fallback label.
    });
}
