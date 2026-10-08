/*
 * Aggregate barrel for the api/ domain modules. Pages import { api } from here;
 * endpoint implementations live in api/<domain>.ts (see http.ts for the rules).
 */
import * as healthApi from './api/health'
import * as itemsApi from './api/items'

export const api = {
  ...healthApi,
  ...itemsApi,
}
