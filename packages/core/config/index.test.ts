import { beforeEach, describe, expect, it } from "vitest";
import { configStore } from "./index";

describe("configStore.setWebhookRuntimeConfig", () => {
  beforeEach(() => {
    configStore.getState().setWebhookRuntimeConfig({});
  });

  it("defaults every missing value to an empty string", () => {
    configStore.getState().setWebhookRuntimeConfig({});
    const s = configStore.getState();
    expect(s.webhookRuntimeDispatchUrl).toBe("");
    expect(s.webhookRuntimeEventType).toBe("");
    expect(s.webhookRuntimeRunnerRepo).toBe("");
    expect(s.webhookRuntimeRunnerPath).toBe("");
  });

  it("stores the values a configured server sends", () => {
    configStore.getState().setWebhookRuntimeConfig({
      webhookRuntimeDispatchUrl: "http://t:8090/v1/dispatch",
      webhookRuntimeEventType: "multica-task",
      webhookRuntimeRunnerRepo: "acme/pipeline",
      webhookRuntimeRunnerPath: "deployment/local-runner",
    });
    const s = configStore.getState();
    expect(s.webhookRuntimeDispatchUrl).toBe("http://t:8090/v1/dispatch");
    expect(s.webhookRuntimeEventType).toBe("multica-task");
    expect(s.webhookRuntimeRunnerRepo).toBe("acme/pipeline");
    expect(s.webhookRuntimeRunnerPath).toBe("deployment/local-runner");
  });

  it("resets a previously set value when the next config omits it", () => {
    configStore.getState().setWebhookRuntimeConfig({ webhookRuntimeDispatchUrl: "http://t" });
    configStore.getState().setWebhookRuntimeConfig({});
    expect(configStore.getState().webhookRuntimeDispatchUrl).toBe("");
  });
});
