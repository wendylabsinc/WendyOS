import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { Callout } from 'fumadocs-ui/components/callout';
import { Card, Cards } from 'fumadocs-ui/components/card';
import { Pre } from 'fumadocs-ui/components/codeblock';
import { File, Files, Folder } from 'fumadocs-ui/components/files';
import { Step, Steps } from 'fumadocs-ui/components/steps';
import { Tab, Tabs } from 'fumadocs-ui/components/tabs';
import { TypeTable } from 'fumadocs-ui/components/type-table';
import defaultMdxComponents from 'fumadocs-ui/mdx';
import type { MDXComponents } from 'mdx/types';
import { Asciicast } from './docs/asciicast';
import {
  JetsonBuyersTable,
  Pi5BuyersTable,
  PiZero2WBuyersTable,
} from './docs/buyers-tables';
import { CliClip, CliShot } from './docs/cli-shot';
import { CliToAgentRelationshipDiagram } from './docs/cli-to-agent-relationship-diagram';
import { Figure, Frame, Frames } from './docs/figure';
import { Flow, FlowArrow, FlowNode, FlowPart } from './docs/flow';
import { HardwareCard, HardwareShowcase } from './docs/hardware-showcase';
import { TrackedCodeBlock } from './docs/install-copy-tracking';
import { Path, Paths } from './docs/paths';
import { SetDefaultDeviceSection } from './docs/set-default-device-section';
import { YouTubeVideo } from './docs/youtube-video';

export function getMDXComponents(components?: MDXComponents) {
  return {
    ...defaultMdxComponents,
    pre: (props) => (
      <TrackedCodeBlock {...props}>
        <Pre>{props.children}</Pre>
      </TrackedCodeBlock>
    ),
    Accordion,
    Accordions,
    Asciicast,
    Callout,
    Card,
    Cards,
    Figure,
    File,
    Files,
    Flow,
    FlowArrow,
    FlowNode,
    FlowPart,
    Folder,
    Frame,
    Frames,
    HardwareCard,
    HardwareShowcase,
    JetsonBuyersTable,
    Path,
    Paths,
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
