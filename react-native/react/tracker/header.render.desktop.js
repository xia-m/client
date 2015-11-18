'use strict'
/* @flow */

// $FlowIssue base-react
import React, {Component} from '../base-react'
import {AppBar, IconButton} from 'material-ui'
import NavigationClose from 'material-ui/lib/svg-icons/navigation/close'

export type HeaderProps = {
  reason: string,
  onClose: () => void
}

export default class HeaderRender extends Component {
  props: HeaderProps;

  render (): ReactElement {
    return (
      <AppBar
        style={{userSelect: 'none', cursor: 'default'}}
        title={this.props.reason}
        iconElementLeft={<div/>}
        iconElementRight={
          <IconButton onTouchTap={() => this.props.onClose()}>
            <NavigationClose />
          </IconButton>
        }
        />
    )
  }
}

HeaderRender.propTypes = {
  reason: React.PropTypes.string.isRequired,
  onClose: React.PropTypes.func.isRequired
}
