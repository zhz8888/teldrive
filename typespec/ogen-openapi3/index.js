import { emitFile, resolvePath } from "@typespec/compiler";
import { getOpenAPI3 } from "@typespec/openapi3";
import { stringify } from "yaml";

// rawResponseMedia lists the operations whose response bodies are not JSON, so
// the emitter can mark them for ogen: a download answers a byte stream and the
// event stream answers text/event-stream, and without the marker ogen would try
// to decode them as the schema the TypeSpec operation declares.
const rawResponseMedia = new Map([
  ["downloadFile", [["200", "application/octet-stream"], ["206", "application/octet-stream"]]],
  ["downloadPublicShare", [["200", "application/octet-stream"], ["206", "application/octet-stream"]]],
  ["downloadPublicShareFile", [["200", "application/octet-stream"], ["206", "application/octet-stream"]]],
  ["downloadFileLegacy", [["200", "application/octet-stream"], ["206", "application/octet-stream"]]],
  ["downloadPublicShareLegacy", [["200", "application/octet-stream"], ["206", "application/octet-stream"]]],
  ["downloadPublicShareFileLegacy", [["200", "application/octet-stream"], ["206", "application/octet-stream"]]],
  ["streamEvents", [["200", "text/event-stream"]]],
]);

// $onEmit is the emitter entry point TypeSpec calls after compilation: it builds
// the OpenAPI documents, surfaces their diagnostics, and writes one file per
// service version unless the run is a dry run or already failed.
export async function $onEmit(context) {
  const services = await getOpenAPI3(context.program, context.options);

  for (const service of services) {
    if (service.versioned) {
      for (const version of service.versions) {
        context.program.reportDiagnostics(version.diagnostics);
      }
    } else {
      context.program.reportDiagnostics(service.diagnostics);
    }
  }

  if (context.program.compilerOptions.dryRun || context.program.hasError()) {
    return;
  }

  for (const service of services) {
    if (service.versioned) {
      for (const version of service.versions) {
        await emitDocument(context, version.document);
      }
    } else {
      await emitDocument(context, service.document);
    }
  }
}

// emitDocument marks the raw responses and writes the document as the emitter's
// single output file, whose name and line endings come from tspconfig.yaml.
async function emitDocument(context, document) {
  markOgenRawResponses(document);
  await emitFile(context.program, {
    path: resolvePath(context.emitterOutputDir, context.options["output-file"] ?? "openapi.yaml"),
    content: stringify(document, { aliasDuplicateObjects: false }),
    newLine: context.options["new-line"],
  });
}

// markOgenRawResponses adds the x-ogen-raw-response extension to every response
// body listed in rawResponseMedia, which is how ogen is told to hand the body to
// the handler as a stream instead of decoding it.
function markOgenRawResponses(document) {
  for (const pathItem of Object.values(document.paths ?? {})) {
    for (const operation of Object.values(pathItem ?? {})) {
      const mediaTypes = rawResponseMedia.get(operation?.operationId);
      if (!mediaTypes) {
        continue;
      }
      for (const [status, contentType] of mediaTypes) {
        const media = operation.responses?.[status]?.content?.[contentType];
        if (media) {
          media["x-ogen-raw-response"] = true;
        }
      }
    }
  }
}
