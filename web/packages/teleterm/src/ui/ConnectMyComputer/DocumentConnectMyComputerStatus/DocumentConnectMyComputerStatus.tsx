/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import React from 'react';
import {
  Alert,
  Box,
  ButtonPrimary,
  Flex,
  Label,
  Link,
  MenuItem,
  Text,
} from 'design';
import styled, { css } from 'styled-components';
import { Transition } from 'react-transition-group';

import { makeLabelTag } from 'teleport/components/formatters';
import { MenuIcon } from 'shared/components/MenuAction';
import { CircleCheck, Laptop, Moon, Warning } from 'design/Icon';
import Indicator from 'design/Indicator';

import {
  AgentProcessError,
  CurrentAction,
  useConnectMyComputerContext,
} from 'teleterm/ui/ConnectMyComputer';
import Document from 'teleterm/ui/Document';
import * as types from 'teleterm/ui/services/workspacesService';
import { useWorkspaceContext } from 'teleterm/ui/Documents';
import { assertUnreachable } from 'teleterm/ui/utils';
import { codeOrSignal } from 'teleterm/ui/utils/process';

import { useAgentProperties } from '../useAgentProperties';
import { Logs } from '../Logs';

import type * as tsh from 'teleterm/services/tshd/types';
import type { IconProps } from 'design/Icon/Icon';

interface DocumentConnectMyComputerStatusProps {
  visible: boolean;
  doc: types.DocumentConnectMyComputerStatus;
}

// When the documents are reopened, doc.connect_my_computer_status is replaced
// with doc.connect_my_computer_setup.
// This is needed to prevent showing the status document when the agent is not configured
// (or its configuration has been removed).
//
// Then we check if the agent is configured, and if it is, we replace setup
// with the status document.
export function DocumentConnectMyComputerStatus(
  props: DocumentConnectMyComputerStatusProps
) {
  const {
    currentAction,
    agentNode,
    downloadAndStartAgent,
    killAgent,
    isAgentConfiguredAttempt,
  } = useConnectMyComputerContext();
  const { documentsService, rootClusterUri } = useWorkspaceContext();
  const { roleName, systemUsername, hostname } = useAgentProperties();

  const prettyCurrentAction = prettifyCurrentAction(currentAction);

  function replaceWithSetupDocument(): void {
    documentsService.replace(
      props.doc.uri,
      documentsService.createConnectMyComputerSetupDocument({
        rootClusterUri,
      })
    );
  }

  const isRunning =
    currentAction.kind === 'observe-process' &&
    currentAction.agentProcessState.status === 'running';
  const isKilling =
    currentAction.kind === 'kill' &&
    currentAction.attempt.status === 'processing';
  const isDownloading =
    currentAction.kind === 'download' &&
    currentAction.attempt.status === 'processing';
  const isStarting =
    currentAction.kind === 'start' &&
    currentAction.attempt.status === 'processing';

  const showDisconnectButton = isRunning || isKilling;
  const disableDisconnectButton = isKilling;
  const disableConnectButton = isDownloading || isStarting;

  return (
    <Document visible={props.visible}>
      <Box maxWidth="590px" mx="auto" mt="4" px="5" width="100%">
        {isAgentConfiguredAttempt.status === 'error' && (
          <Alert
            css={`
              display: block;
            `}
          >
            An error occurred while reading the agent config file:{' '}
            {isAgentConfiguredAttempt.statusText}. <br />
            You can try to{' '}
            <Link
              onClick={replaceWithSetupDocument}
              css={`
                cursor: pointer;
              `}
            >
              run the setup
            </Link>{' '}
            again.
          </Alert>
        )}
        <Flex justifyContent="space-between" mb={3}>
          <Text
            typography="h3"
            css={`
              display: flex;
            `}
          >
            <Laptop mr={2} />
            {/** The node name can be changed, so it might be different from the system hostname. */}
            {agentNode?.hostname || hostname}
          </Text>
          <MenuIcon
            buttonIconProps={{
              css: css`
                border-radius: ${props => props.theme.space[1]}px;
                background: ${props => props.theme.colors.spotBackground[0]};
              `,
            }}
            menuProps={{
              anchorOrigin: {
                vertical: 'bottom',
                horizontal: 'right',
              },
              transformOrigin: {
                vertical: 'top',
                horizontal: 'right',
              },
            }}
          >
            <MenuItem onClick={() => alert('Not implemented')}>
              Remove agent
            </MenuItem>
          </MenuIcon>
        </Flex>

        <Transition in={!!agentNode} timeout={1_800} mountOnEnter>
          {state => (
            <LabelsContainer gap={1} className={state}>
              {renderLabels(agentNode.labelsList)}
            </LabelsContainer>
          )}
        </Transition>
        <Flex mt={3} mb={2} gap={1} display="flex" alignItems="center">
          {prettyCurrentAction.Icon && (
            <prettyCurrentAction.Icon size="medium" />
          )}
          {prettyCurrentAction.title}
        </Flex>
        {prettyCurrentAction.error && (
          <Alert
            css={`
              white-space: pre-wrap;
            `}
          >
            {prettyCurrentAction.error}
          </Alert>
        )}
        {prettyCurrentAction.logs && <Logs logs={prettyCurrentAction.logs} />}
        <Text mb={4} mt={1}>
          Connecting your computer will allow any cluster user with the role{' '}
          <strong>{roleName}</strong> to access it as an SSH resource with the
          user <strong>{systemUsername}</strong>.
        </Text>
        {showDisconnectButton ? (
          <ButtonPrimary
            block
            disabled={disableDisconnectButton}
            onClick={killAgent}
          >
            Disconnect
          </ButtonPrimary>
        ) : (
          <ButtonPrimary
            block
            disabled={disableConnectButton}
            onClick={downloadAndStartAgent}
          >
            Connect
          </ButtonPrimary>
        )}
      </Box>
    </Document>
  );
}

function renderLabels(labelsList: tsh.Label[]): JSX.Element[] {
  const labels = labelsList.map(makeLabelTag);
  return labels.map(label => (
    <Label key={label} kind="secondary">
      {label}
    </Label>
  ));
}

function prettifyCurrentAction(currentAction: CurrentAction): {
  Icon: React.FC<IconProps>;
  title: string;
  error?: string;
  logs?: string;
} {
  const noop = {
    Icon: StyledIndicator,
    title: '',
  };

  switch (currentAction.kind) {
    case 'download': {
      switch (currentAction.attempt.status) {
        case '':
        case 'processing': {
          // TODO(gzdunek) add progress
          return {
            Icon: StyledIndicator,
            title: 'Verifying agent binary',
          };
        }
        case 'error': {
          return {
            Icon: StyledWarning,
            title: 'Failed to download agent',
            error: currentAction.attempt.statusText,
          };
        }
        case 'success': {
          return noop; // noop, not used, at this point it should be start processing.
        }
        default: {
          return assertUnreachable(currentAction.attempt.status);
        }
      }
    }
    case 'start': {
      switch (currentAction.attempt.status) {
        case '':
        case 'processing': {
          return {
            Icon: StyledIndicator,
            title: 'Starting',
          };
        }
        case 'error': {
          if (currentAction.attempt.statusText !== AgentProcessError.name) {
            return {
              Icon: StyledWarning,
              title: 'Failed to start agent',
              error: currentAction.attempt.statusText,
            };
          }

          if (currentAction.agentProcessState.status === 'error') {
            return {
              Icon: StyledWarning,
              title:
                'Failed to start agent – an error occurred while spawning the agent process',
              error: currentAction.agentProcessState.message,
            };
          }

          if (currentAction.agentProcessState.status === 'exited') {
            const { code, signal } = currentAction.agentProcessState;
            return {
              Icon: StyledWarning,
              title: `Failed to start agent - the agent process quit unexpectedly with ${codeOrSignal(
                code,
                signal
              )}`,
              logs: currentAction.agentProcessState.logs,
            };
          }
          break;
        }
        case 'success': {
          return noop; // noop, not used, at this point it should be observe-process running.
        }
        default: {
          return assertUnreachable(currentAction.attempt.status);
        }
      }
      break;
    }
    case 'observe-process': {
      switch (currentAction.agentProcessState.status) {
        case 'not-started': {
          return {
            Icon: Moon,
            title: 'Agent not running',
          };
        }
        case 'running': {
          return {
            Icon: props => <CircleCheck {...props} color="success" />,
            title: 'Agent running',
          };
        }
        case 'exited': {
          const { code, signal, exitedSuccessfully } =
            currentAction.agentProcessState;

          if (exitedSuccessfully) {
            return {
              Icon: Moon,
              title: 'Agent not running',
            };
          } else {
            return {
              Icon: StyledWarning,
              title: `Agent process exited with ${codeOrSignal(code, signal)}`,
              logs: currentAction.agentProcessState.logs,
            };
          }
        }
        case 'error': {
          // TODO(ravicious): This can happen only just before killing the process. 'error' should
          // not be considered a separate process state. See the comment above the 'error' status
          // definition.
          return {
            Icon: StyledWarning,
            title: 'An error occurred to agent process',
            error: currentAction.agentProcessState.message,
          };
        }
        default: {
          return assertUnreachable(currentAction.agentProcessState);
        }
      }
    }
    case 'kill': {
      switch (currentAction.attempt.status) {
        case '':
        case 'processing': {
          return {
            Icon: StyledIndicator,
            title: 'Stopping',
          };
        }
        case 'error': {
          return {
            Icon: StyledWarning,
            title: 'Failed to stop agent',
            error: currentAction.attempt.statusText,
          };
        }
        case 'success': {
          return noop; // noop, not used, at this point it should be observe-process exited.
        }
        default: {
          return assertUnreachable(currentAction.attempt.status);
        }
      }
    }
  }
}

const StyledWarning = styled(Warning).attrs({
  color: 'error.main',
})``;

const StyledIndicator = styled(Indicator).attrs({ delay: 'none' })`
  color: inherit;
  display: inline-flex;
`;

const LabelsContainer = styled(Flex)`
  &.entering {
    animation-duration: 1.8s;
    animation-name: lineInserted;
    animation-timing-function: ease-in;
    overflow: hidden;
    animation-fill-mode: forwards;
    // We don't know the height of labels, so we animate max-height instead of height
    @keyframes lineInserted {
      from {
        max-height: 0;
      }
      to {
        max-height: 100%;
      }
    }
  }
`;
