import React, { Component, useState, useEffect } from 'react';
import PropTypes from 'prop-types';
import { makeStyles } from '@material-ui/core/styles';
import { Container } from '@material-ui/core';
import Box from '@material-ui/core/Box';
import Collapse from '@material-ui/core/Collapse';
import IconButton from '@material-ui/core/IconButton';
import Table from '@material-ui/core/Table';
import TableBody from '@material-ui/core/TableBody';
import TableCell from '@material-ui/core/TableCell';
import TableContainer from '@material-ui/core/TableContainer';
import TableHead from '@material-ui/core/TableHead';
import TableRow from '@material-ui/core/TableRow';
import Typography from '@material-ui/core/Typography';
import Paper from '@material-ui/core/Paper';
import KeyboardArrowDownIcon from '@material-ui/icons/KeyboardArrowDown';
import KeyboardArrowUpIcon from '@material-ui/icons/KeyboardArrowUp';

import Moment from 'react-moment';

import {grpc} from "@improbable-eng/grpc-web";
import { ManagerUI } from './scarab-ui_pb_service';
import { ListProfilesRequest, ListProfilesResponse } from './scarab-ui_pb';
import { RegisteredProfile } from './scarab-common_pb';
import { ManagerUIClient } from './scarab-ui_pb_service';

const host = "http://localhost:8080";

const useRowStyles = makeStyles({
    root: {
          '& > *': {
                  borderBottom: 'unset',
                  },
          },
});

type RowProps = {
  profile: RegisteredProfile
};

function Row(props: RowProps){
    const [open, setOpen] = React.useState(false);
    const {profile} = props
    const spec = profile.getSpec()
    const firstReg = profile.getFirstregistration()
    const workers = profile.getWorkersList()
    const classes = useRowStyles();

    return (
          <React.Fragment>
            <TableRow className={classes.root}>
              <TableCell>
                <IconButton aria-label="expand row" size="small" onClick={() => setOpen(!open)}>
                  {open ? <KeyboardArrowUpIcon /> : <KeyboardArrowDownIcon />}
                </IconButton>
              </TableCell>
              <TableCell component="th" scope="row">
                {spec?.getProfile() ?? "unknown"}
              </TableCell>
              <TableCell component="th" scope="row">
                {spec?.getVersion() ?? "unknown"}
              </TableCell>
              <TableCell component="th" scope="row">
                <Moment>{firstReg.toDate()}</Moment>
              </TableCell>
              <TableCell component="th" scope="row">
                {workers?.length ?? 0}
              </TableCell>
            </TableRow>
          </React.Fragment>
        );
}

type PTProps = {};

type PTState = {
    profiles: RegisteredProfile[],
    error: boolean
};

const ProfilesTable: React.FC<PTProps> = (props: PTProps) => {
  const [state, setState] = useState<PTState>({
      profiles: [],
      error: false,
  })

  useEffect(() => {
    const req = new ListProfilesRequest();
    const client = new ManagerUIClient(host);
    client.listProfiles(req, (err, lpResp) => {
      if (lpResp != null) {
        setState({
          profiles: lpResp.getRegisteredList(),
          error: false
        })
      }
    });
  },[])

  return (
    <TableContainer component={Paper}>
      <Table aria-label="collapsible table">
        <TableHead>
          <TableRow>
            <TableCell />
            <TableCell>Name</TableCell>
            <TableCell>Version</TableCell>
            <TableCell>FirstRegistration</TableCell>
            <TableCell>Workers</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {state.profiles.map((row :RegisteredProfile) => {
            return (
              <Row profile={row} />
            )
          })}
        </TableBody>
      </Table>
    </TableContainer>
  );
}


export default ProfilesTable;
