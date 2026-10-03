import path from "node:path";
import { readdir, readFile, writeFile } from "node:fs/promises";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react, { reactCompilerPreset } from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import Icons from "unplugin-icons/vite";
import { defineConfig, type Plugin } from "vite";
import babel from "@rolldown/plugin-babel";

const pdfJsRoot = path.resolve(import.meta.dirname, "node_modules/pdfjs-dist");
const pdfJsAssetDirectories = ["cmaps", "standard_fonts", "wasm", "iccs"] as const;

async function assetFiles(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = await Promise.all(
    entries.map((entry) => {
      const file = path.join(directory, entry.name);
      return entry.isDirectory() ? assetFiles(file) : [file];
    }),
  );
  return files.flat();
}

// keepDistPlaceholder restores the tracked file that makes the output directory
// exist in a fresh clone, which is what go:embed needs to compile before the
// interface has ever been built. The build empties its output directory, so the
// file is rewritten when the bundle closes, whichever command started the build.
function keepDistPlaceholder(): Plugin {
  const outputDirectory = path.resolve(import.meta.dirname, "dist");
  return {
    name: "keep-dist-placeholder",
    async closeBundle() {
      await writeFile(path.join(outputDirectory, ".gitkeep"), "");
    },
  };
}

function pdfJsAssets(): Plugin {
  return {
    name: "pdfjs-assets",
    enforce: "pre" as const,
    transform(code: string, id: string) {
      if (id.includes("/foliate-js/view.js")) {
        return code.replace(
          `    else if (await isPDF(file)) {
        const { makePDF } = await import('./pdf.js')
        book = await makePDF(file)
    }`,
          `    else if (await isPDF(file)) {
        throw new UnsupportedTypeError('PDF files use the Teldrive PDF reader')
    }`,
        );
      }
      if (id.includes("/foliate-js/paginator.js")) {
        let patched = code.replace(
          "#observer = new ResizeObserver(() => this.expand())",
          "#observer = new ResizeObserver(entries => { if (entries.some(entry => entry.target.isConnected)) this.expand() })",
        );
        patched = patched.replace(
          "#observer = new ResizeObserver(() => this.render())",
          "#observer = new ResizeObserver(entries => { if (entries.some(entry => entry.target.isConnected)) this.render() })",
        );
        patched = patched.replace(
          "    render(layout) {\n        if (!layout) return",
          "    render(layout) {\n        if (!layout || !this.document) return",
        );
        patched = patched.replace(
          "    render() {\n        if (!this.#view) return",
          "    render() {\n        if (!this.#view?.document) return",
        );
        return patched;
      }
    },
    async generateBundle() {
      for (const directoryName of pdfJsAssetDirectories) {
        const directory = path.join(pdfJsRoot, directoryName);
        for (const file of await assetFiles(directory)) {
          this.emitFile({
            type: "asset",
            fileName: `pdfjs/${directoryName}/${path.relative(directory, file).split(path.sep).join("/")}`,
            source: await readFile(file),
          });
        }
      }
    },
  };
}

export default defineConfig(() => {
  const backendAddress = process.env.TELDRIVE_HTTP_ADDRESS ?? "127.0.0.1:8080";
  const backendHost = backendAddress.startsWith("0.0.0.0:")
    ? `127.0.0.1:${backendAddress.slice("0.0.0.0:".length)}`
    : backendAddress;

  return {
    plugins: [
      keepDistPlaceholder(),
      pdfJsAssets(),
      tanstackRouter(),
      react(),
      babel({ presets: [reactCompilerPreset()] }),
      tailwindcss(),
      Icons({
        compiler: "jsx",
        jsx: "react",
        autoInstall: true,
        iconCustomizer(_1, _2, props) {
          props.width = "1.25rem";
          props.height = "1.25rem";
          props.className = "pointer-events-none";
        },
      }),
    ],
    resolve: {
      dedupe: ["react", "react-dom"],
      alias: {
        "@": path.resolve(import.meta.dirname, "./src"),
      },
    },
    optimizeDeps: {
      exclude: ["foliate-js"],
    },
    server: {
      cors: true,
      proxy: {
        "/api": {
          target: `http://${backendHost}`,
        },
      },
    },
  };
});
