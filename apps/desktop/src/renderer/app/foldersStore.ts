/**
 * User-scoped custom VM folders. Purely local (localStorage), like favorites
 * and recents — never leaves the machine, never touches the runtime/API.
 * Folders are keyed per endpoint so each cluster has its own set.
 */

export interface VMFolder {
  id: string;
  name: string;
  vmIds: string[];
  /** Optional accent color (CSS value). Undefined = no color / neutral. */
  color?: string;
}

const KEY = 'ahv-vm-folders';

/** Stored shape: { [endpointId]: VMFolder[] }. */
type FolderMap = Record<string, VMFolder[]>;

function readAll(): FolderMap {
  try {
    return JSON.parse(localStorage.getItem(KEY) ?? '{}') as FolderMap;
  } catch {
    return {};
  }
}

function writeAll(map: FolderMap): void {
  localStorage.setItem(KEY, JSON.stringify(map));
}

export function getFolders(epId: string | undefined): VMFolder[] {
  if (!epId) return [];
  return readAll()[epId] ?? [];
}

function setFoldersFor(epId: string, folders: VMFolder[]): VMFolder[] {
  const map = readAll();
  map[epId] = folders;
  writeAll(map);
  return folders;
}

export function createFolder(epId: string, name: string, color?: string): VMFolder[] {
  const trimmed = name.trim();
  if (!trimmed) return getFolders(epId);
  const folder: VMFolder = { id: crypto.randomUUID(), name: trimmed, vmIds: [], color };
  return setFoldersFor(epId, [...getFolders(epId), folder]);
}

/** Create a folder seeded with one VM — used by the per-tile "new folder" row. */
export function createFolderWithVm(
  epId: string,
  name: string,
  vmId: string,
  color?: string,
): VMFolder[] {
  const trimmed = name.trim();
  if (!trimmed) return getFolders(epId);
  const folder: VMFolder = { id: crypto.randomUUID(), name: trimmed, vmIds: [vmId], color };
  return setFoldersFor(epId, [...getFolders(epId), folder]);
}

export function deleteFolder(epId: string, folderId: string): VMFolder[] {
  return setFoldersFor(
    epId,
    getFolders(epId).filter((f) => f.id !== folderId),
  );
}

export function toggleVmInFolder(epId: string, folderId: string, vmId: string): VMFolder[] {
  return setFoldersFor(
    epId,
    getFolders(epId).map((f) => {
      if (f.id !== folderId) return f;
      const has = f.vmIds.includes(vmId);
      return { ...f, vmIds: has ? f.vmIds.filter((x) => x !== vmId) : [...f.vmIds, vmId] };
    }),
  );
}
