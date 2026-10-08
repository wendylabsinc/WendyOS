import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { Callout } from 'fumadocs-ui/components/callout';
import { Card, Cards } from 'fumadocs-ui/components/card';
import { File, Files, Folder } from 'fumadocs-ui/components/files';
import { Step, Steps } from 'fumadocs-ui/components/steps';
import { Tab, Tabs } from 'fumadocs-ui/components/tabs';
import { TypeTable } from 'fumadocs-ui/components/type-table';
import defaultMdxComponents from 'fumadocs-ui/mdx';
import type { MDXComponents } from 'mdx/types';
import {
  JetsonBuyersTable,
  Pi5BuyersTable,
  PiZero2WBuyersTable,
} from './docs/buyers-tables';
import { CliClip, CliShot } from './docs/cli-shot';
import { CliToAgentRelationshipDiagram } from './docs/cli-to-agent-relationship-diagram';
import { SetDefaultDeviceSection } from './docs/set-default-device-section';
import { YouTubeVideo } from './docs/youtube-video';

export function getMDXComponents(components?: MDXComponents) {
  return {
    ...defaultMdxComponents,
    Accordion,
    Accordions,
    Callout,
    Card,
    Cards,
    File,
    Files,
    Folder,
    JetsonBuyersTable,
    Pi5BuyersTable,
    PiZero2WBuyersTable,
    Step,
    Steps,
    Tab,
    Tabs,
    TypeTable,
    CliClip,
    CliShot,
    CliToAgentRelationshipDiagram,
    SetDefaultDeviceSection,
    YouTubeVideo,
    ...components,
  } satisfies MDXComponents;
}

export const useMDXComponents = getMDXComponents;

declare global {
  type MDXProvidedComponents = ReturnType<typeof getMDXComponents>;
}
