'use client';

import { Terminal, X } from 'lucide-react';
import { useRef } from 'react';
import {
  agentCurlCommand,
  cliCurlCommand,
  cliWingetCommand,
  InstallCommand,
} from '@/components/docs/install-command';

export function InstallScripts() {
  const dialogRef = useRef<HTMLDialogElement>(null);

  // Native <dialog> rendered with showModal() lives in the browser top layer,
  // so it covers the whole viewport regardless of any transformed/overflow
  // ancestor (e.g. the sidebar this trigger sits in).
  return (
    <>
      <button
        type="button"
        onClick={() => dialogRef.current?.showModal()}
        className="inline-flex items-center gap-1.5 px-2 py-1 text-sm text-fd-muted-foreground transition-colors hover:bg-fd-accent hover:text-fd-accent-foreground"
      >
        <Terminal className="size-4" />
        Install
      </button>

      <dialog
        ref={dialogRef}
        aria-label="Install Wendy"
        onClick={(e) => {
          if (e.target === dialogRef.current) dialogRef.current?.close();
        }}
        className="m-auto w-full max-w-lg border bg-fd-popover p-0 text-fd-popover-foreground shadow-lg backdrop:bg-black/50 sm:max-w-xl"
      >
        <div className="relative">
          <button
            type="button"
            aria-label="Close"
            onClick={() => dialogRef.current?.close()}
            className="absolute right-3 top-3 p-1 text-fd-muted-foreground transition-colors hover:text-fd-foreground"
          >
            <X className="size-4" />
          </button>

          <div className="max-h-[85vh] overflow-y-auto p-6">
            <h2 className="text-lg font-semibold">Install Wendy</h2>

            <section className="mt-5">
              <h3 className="font-medium">Install Wendy CLI</h3>
              <p className="mt-1 text-sm text-fd-muted-foreground">
                Install this on your developer machine or continuous integration machine.
              </p>
              <InstallCommand
                analyticsEventName="cli_install_copy"
                analyticsEventParams={{
                  install_target: 'unix',
                  install_variant: 'cli for macOS/linux',
                  install_label: 'macOS/Linux CLI',
                  location: 'docs_install_scripts_dialog',
                }}
                label="macOS / Linux"
                command={cliCurlCommand}
              />
              <InstallCommand
                analyticsEventName="cli_install_copy"
                analyticsEventParams={{
                  install_target: 'windows',
                  install_variant: 'cli for windows',
                  install_label: 'windows CLI',
                  location: 'docs_install_scripts_dialog',
                }}
                label="Windows"
                command={cliWingetCommand}
              />
            </section>

            <section className="mt-6">
              <h3 className="font-medium">
                Install <code className="font-mono text-[0.95em]">wendy-agent</code>
              </h3>
              <p className="mt-1 text-sm text-fd-muted-foreground">
                Install this on your Linux machine. You do <strong>not</strong> need to do this for
                WendyOS — it&apos;s already there!
              </p>
              <InstallCommand
                analyticsEventName="cli_install_copy"
                analyticsEventParams={{
                  install_target: 'agent-linux',
                  install_variant: 'wendy-agent for Linux',
                  install_label: 'wendy-agent for Linux',
                  location: 'docs_install_scripts_dialog',
                }}
                label="Linux"
                command={agentCurlCommand}
              />
            </section>
          </div>
        </div>
      </dialog>
    </>
  );
}
