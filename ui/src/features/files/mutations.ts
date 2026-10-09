import { useQueryClient } from "@tanstack/react-query";
import { $api } from "@/api/client";
import type { FileEntry, NameConflictPolicy } from "@/api/types";

/**
 * File-tree mutations for the browser and the trash page. Every action invalidates the
 * affected queries before it resolves, so a caller that awaits one does not have to refresh
 * the tree itself. `pending` and `error` are derived from the mutation objects below and
 * therefore cover exactly these actions.
 */
export function useFileActions() {
  const queryClient = useQueryClient();
  const createFolderMutation = $api.useMutation("post", "/v1/folders");
  const renameMutation = $api.useMutation("patch", "/v1/files/{fileId}");
  const moveMutation = $api.useMutation("post", "/v1/files/{fileId}/move");
  const copyMutation = $api.useMutation("post", "/v1/files/{fileId}/copy");
  const trashMutation = $api.useMutation("delete", "/v1/files/{fileId}");
  const restoreMutation = $api.useMutation("post", "/v1/files/{fileId}/restore");
  const purgeMutation = $api.useMutation("delete", "/v1/files/{fileId}/purge");

  const cleanTrashMutation = $api.useMutation("delete", "/v1/files/trash");
  const bulkMoveMutation = $api.useMutation("post", "/v1/files/bulk/move");
  const bulkTrashMutation = $api.useMutation("post", "/v1/files/bulk/trash");

  /**
   * Drops every cached query a file mutation can invalidate: the listings, the single-file
   * query and the drive statistics the sidebar shows. Awaited so the refresh has been
   * scheduled before the action resolves.
   */
  async function invalidateFiles() {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["get", "/v1/files"] }),
      queryClient.invalidateQueries({ queryKey: ["get", "/v1/files/{fileId}"] }),
      queryClient.invalidateQueries({ queryKey: ["get", "/v1/files/statistics/drive"] }),
    ]);
  }

  /** Creates a folder under `parentId` (drive root when omitted); an existing name fails. */
  async function createFolder(name: string, parentId?: string) {
    const result = await createFolderMutation.mutateAsync({
      body: { parentId, name, conflictPolicy: "fail" },
    });
    await invalidateFiles();
    return result;
  }

  /**
   * Renames a file or folder. The `If-Match` header carries the generation the row was
   * rendered from, so a concurrent change fails the request instead of being overwritten.
   */
  async function rename(file: FileEntry, name: string) {
    const result = await renameMutation.mutateAsync({
      params: {
        path: { fileId: file.id },
        header: { "If-Match": `"${file.generation}"` },
      },
      body: { name },
    });
    await invalidateFiles();
    return result;
  }

  /**
   * Moves one entry into `parentId` (drive root when omitted), guarded by the same
   * generation precondition as {@link rename}. `conflictPolicy` decides what a name clash
   * in the destination does; the default fails the request.
   */
  async function move(
    file: FileEntry,
    parentId?: string,
    conflictPolicy: NameConflictPolicy = "fail",
  ) {
    const result = await moveMutation.mutateAsync({
      params: {
        path: { fileId: file.id },
        header: {
          "If-Match": `"${file.generation}"`,
        },
      },
      body: { parentId, conflictPolicy },
    });
    await invalidateFiles();
    return result;
  }

  /**
   * Copies one entry, optionally under a new name ("name copy" is what the duplicate
   * action passes). Unlike move there is no precondition: a copy overwrites nothing
   * unless `conflictPolicy` says to.
   */
  async function copy(
    file: FileEntry,
    parentId?: string,
    name?: string,
    conflictPolicy: NameConflictPolicy = "fail",
  ) {
    const result = await copyMutation.mutateAsync({
      params: {
        path: { fileId: file.id },
      },
      body: { parentId, name, conflictPolicy },
    });
    await invalidateFiles();
    return result;
  }

  /**
   * Copies several entries into one destination. The copies run as one request per file in
   * parallel, so a failure part-way leaves the already-copied entries in place and rejects
   * with the first error. The listings are invalidated even when one copy failed, because
   * the ones that did land are on the server already: skipping the refresh would hide them
   * and make the caller's retry copy them a second time.
   */
  async function copyMany(
    files: FileEntry[],
    parentId?: string,
    conflictPolicy: NameConflictPolicy = "fail",
  ) {
    try {
      return await Promise.all(
        files.map((file) =>
          copyMutation.mutateAsync({
            params: {
              path: { fileId: file.id },
            },
            body: { parentId, conflictPolicy },
          }),
        ),
      );
    } finally {
      await invalidateFiles();
    }
  }

  /** Moves one entry to the trash; addressed by id, so no generation is required. */
  async function trash(fileId: string) {
    const result = await trashMutation.mutateAsync({ params: { path: { fileId } } });
    await invalidateFiles();
    return result;
  }

  /** Restores one trashed entry and its trashed descendants; the parent must be active. */
  async function restore(fileId: string) {
    const result = await restoreMutation.mutateAsync({
      params: {
        path: { fileId },
      },
    });
    await invalidateFiles();
    return result;
  }

  /** Permanently deletes one trashed entry from the catalog; this cannot be undone. */
  async function purge(fileId: string) {
    const result = await purgeMutation.mutateAsync({ params: { path: { fileId } } });
    await invalidateFiles();
    return result;
  }

  /**
   * Moves the caller's whole trash to deletion pending in one request. The entries leave
   * the trash listing as soon as it refetches; the actual purge happens server-side.
   */
  async function cleanTrash() {
    const result = await cleanTrashMutation.mutateAsync({});
    await invalidateFiles();
    return result;
  }

  /**
   * Moves many entries into one destination in a single transactional request, which
   * (unlike {@link copyMany}) is all-or-nothing. No generation precondition is sent, so a
   * concurrent edit of one of the entries does not fail the move.
   */
  async function bulkMove(
    fileIds: string[],
    parentId?: string,
    conflictPolicy: NameConflictPolicy = "fail",
  ) {
    const result = await bulkMoveMutation.mutateAsync({
      body: { fileIds, parentId, conflictPolicy },
    });
    await invalidateFiles();
    return result;
  }

  /** Moves many entries to the trash in one transactional request. */
  async function bulkTrash(fileIds: string[]) {
    const result = await bulkTrashMutation.mutateAsync({
      body: { fileIds },
    });
    await invalidateFiles();
    return result;
  }

  /**
   * Restores many entries by fanning out one request per id, because the API has no bulk
   * restore; a failure part-way leaves the already-restored entries restored. The listings
   * are invalidated even when one restore failed, for the same reason as {@link copyMany}:
   * the entries that were restored have left the trash and a retry must not repeat them.
   */
  async function bulkRestore(fileIds: string[]) {
    try {
      await Promise.all(
        fileIds.map((fileId) =>
          restoreMutation.mutateAsync({
            params: {
              path: { fileId },
            },
          }),
        ),
      );
    } finally {
      await invalidateFiles();
    }
  }

  // The mutation objects `pending` and `error` aggregate. `bulkRestore` is absent because
  // it reuses `restoreMutation`, which is already listed.
  const mutations = [
    createFolderMutation,
    renameMutation,
    moveMutation,
    copyMutation,
    trashMutation,
    restoreMutation,
    purgeMutation,

    cleanTrashMutation,
    bulkMoveMutation,
    bulkTrashMutation,
  ];

  return {
    createFolder,
    rename,
    move,
    copy,

    copyMany,
    trash,
    restore,
    purge,

    cleanTrash,
    bulkMove,
    bulkTrash,
    bulkRestore,
    pending: mutations.some((mutation) => mutation.isPending),
    error: mutations.find((mutation) => mutation.isError)?.error,
  };
}

/** The action callbacks plus the aggregate `pending`/`error` state, as returned above. */
export type ReturnTypeUseFileActions = ReturnType<typeof useFileActions>;
