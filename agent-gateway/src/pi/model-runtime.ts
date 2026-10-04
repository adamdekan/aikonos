// In-memory Pi model runtime for one agent session.
//
// Pi's ModelRuntime defaults to file-backed credentials (~/.pi/agent/auth.json)
// and model catalogs, plus optional network catalog refreshes. The agent child
// must hold no long-lived secret and share no state, so this runtime keeps
// credentials and catalogs in memory, never reads models.json and never fetches
// a catalog. Callers register their providers explicitly with registerProvider().
import { ModelRuntime, type CreateModelRuntimeOptions } from "@earendil-works/pi-coding-agent";

// pi-ai (which defines these) sits inside Pi's shrinkwrap rather than in our
// node_modules, so the types are derived from the options Pi accepts.
type CredentialStore = NonNullable<CreateModelRuntimeOptions["credentials"]>;
type ModelsStore = NonNullable<CreateModelRuntimeOptions["modelsStore"]>;
type Credential = NonNullable<Awaited<ReturnType<CredentialStore["read"]>>>;
type ModelsStoreEntry = NonNullable<Awaited<ReturnType<ModelsStore["read"]>>>;

// The CredentialStore contract serializes modify/delete per provider id.
function inMemoryCredentialStore(): CredentialStore {
  const credentials = new Map<string, Credential>();
  const chains = new Map<string, Promise<unknown>>();
  function serialize<T>(providerId: string, task: () => Promise<T>): Promise<T> {
    const run = (chains.get(providerId) ?? Promise.resolve()).then(task);
    chains.set(providerId, run.catch(() => undefined));
    return run;
  }
  return {
    read: async (providerId) => credentials.get(providerId),
    list: async () => [...credentials].map(([providerId, credential]) => ({ providerId, type: credential.type })),
    modify: (providerId, fn) =>
      serialize(providerId, async () => {
        const next = await fn(credentials.get(providerId));
        if (next !== undefined) credentials.set(providerId, next);
        return credentials.get(providerId);
      }),
    delete: (providerId) =>
      serialize(providerId, async () => {
        credentials.delete(providerId);
      }),
  };
}

function inMemoryModelsStore(): ModelsStore {
  const entries = new Map<string, ModelsStoreEntry>();
  return {
    read: async (providerId) => entries.get(providerId),
    write: async (providerId, entry) => {
      entries.set(providerId, entry);
    },
    delete: async (providerId) => {
      entries.delete(providerId);
    },
  };
}

/** Creates a Pi ModelRuntime with in-memory credentials and catalogs, no models.json and no network refresh. */
export function createInMemoryModelRuntime(): Promise<ModelRuntime> {
  return ModelRuntime.create({
    credentials: inMemoryCredentialStore(),
    modelsStore: inMemoryModelsStore(),
    modelsPath: null,
    allowModelNetwork: false,
    refreshOnCreate: false,
  });
}
